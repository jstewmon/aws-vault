package vault

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/99designs/keyring"
	"github.com/aws/aws-sdk-go/service/sts"
)

var sessionKeyPattern = regexp.MustCompile(`^session,(?P<profile>[^,]+),(?P<mfaSerial>[^,]*),(?P<expiration>[^:]+)$`)
var oldSessionKeyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^session:(?P<profile>[^ ]+):(?P<mfaSerial>[^ ]*):(?P<expiration>[^:]+)$`),
	regexp.MustCompile(`^(.+?) session \((\d+)\)$`),
}
var base64Encoding = base64.URLEncoding.WithPadding(base64.NoPadding)

func IsSessionKey(s string) bool {
	if sessionKeyPattern.MatchString(s) {
		return true
	}
	for _, pattern := range oldSessionKeyPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

func parseSessionKey(key string, conf *Config) (KeyringSession, error) {
	matches := sessionKeyPattern.FindStringSubmatch(key)
	if len(matches) == 0 {
		return KeyringSession{}, errors.New("failed to parse session name")
	}
	profileName, err := base64Encoding.DecodeString(matches[1])
	if err != nil {
		return KeyringSession{}, err
	}
	mfaSerial, err := base64Encoding.DecodeString(matches[2])
	if err != nil {
		return KeyringSession{}, err
	}
	tsInt, err := strconv.ParseInt(matches[3], 10, 64)
	if err != nil {
		return KeyringSession{}, err
	}
	profile, _ := conf.Profile(string(profileName))
	return KeyringSession{
		Profile:    profile,
		Key:        key,
		Expiration: time.Unix(tsInt, 0),
		MfaSerial:  string(mfaSerial),
	}, nil
}

func formatSessionKey(profile string, mfaSerial string, expiration *time.Time) string {
	return fmt.Sprintf(
		"session,%s,%s,%d",
		base64Encoding.EncodeToString([]byte(profile)),
		base64Encoding.EncodeToString([]byte(mfaSerial)),
		expiration.Unix(),
	)
}

type KeyringSession struct {
	Profile
	Key        string
	Expiration time.Time
	MfaSerial  string
}

func (ks KeyringSession) IsExpired() bool {
	log.Printf("Session %q expires in %v", ks.Key, ks.Expiration.Sub(time.Now()).String())
	return time.Now().After(ks.Expiration)
}

type KeyringSessions struct {
	Keyring keyring.Keyring
	Config  *Config
}

func NewKeyringSessions(k keyring.Keyring, cfg *Config) (*KeyringSessions, error) {
	return &KeyringSessions{
		Keyring: k,
		Config:  cfg,
	}, nil
}

func (s *KeyringSessions) Sessions() ([]KeyringSession, error) {
	log.Printf("Looking up all keys in keyring")
	keys, err := s.Keyring.Keys()
	if err != nil {
		return nil, err
	}

	var sessions []KeyringSession

	for _, k := range keys {
		if IsSessionKey(k) {
			ks, err := parseSessionKey(k, s.Config)
			if err != nil || ks.IsExpired() {
				log.Printf("Session %s is obsolete, attempting deleting", k)
				if err := s.Keyring.Remove(k); err != nil {
					log.Printf("Error deleting session: %v", err)
				}
				continue
			}

			sessions = append(sessions, ks)
		}
	}

	return sessions, nil
}

// Retrieve searches sessions for specific profile, expects the profile to be provided, not the source
func (s *KeyringSessions) Retrieve(profile string, mfaSerial string) (creds sts.Credentials, err error) {
	log.Printf("Looking for sessions for %s", profile)
	sessions, err := s.Sessions()
	if err != nil {
		return creds, err
	}

	for _, session := range sessions {
		if session.Profile.Name == profile && session.MfaSerial == mfaSerial {
			item, err := s.Keyring.Get(session.Key)
			if err != nil {
				return creds, err
			}

			if err = json.Unmarshal(item.Data, &creds); err != nil {
				return creds, err
			}

			// double check the actual expiry time
			if creds.Expiration.Before(time.Now()) {
				log.Printf("Session %q is expired, deleting", session.Key)
				if err = s.Keyring.Remove(session.Profile.Name); err != nil {
					return creds, err
				}
			}

			// success!
			return creds, nil
		}
	}

	return creds, keyring.ErrKeyNotFound
}

// Store stores a sessions for a specific profile, expects the profile to be provided, not the source
func (s *KeyringSessions) Store(profile string, mfaSerial string, session sts.Credentials) error {
	bytes, err := json.Marshal(session)
	if err != nil {
		return err
	}

	key := formatSessionKey(profile, mfaSerial, session.Expiration)
	log.Printf("Writing session for %s to keyring: %q", profile, key)

	return s.Keyring.Set(keyring.Item{
		Key:         key,
		Label:       "aws-vault session for " + profile,
		Description: "aws-vault session for " + profile,
		Data:        bytes,

		// specific Keychain settings
		KeychainNotTrustApplication: false,
	})
}

// Delete deletes any sessions for a specific profile, expects the profile to be provided, not the source
func (s *KeyringSessions) Delete(profile string) (n int, err error) {
	log.Printf("Looking for sessions for %s", profile)
	sessions, err := s.Sessions()
	if err != nil {
		return n, err
	}

	for _, session := range sessions {
		if session.Profile.Name == profile {
			log.Printf("Session %q matches profile %q", session.Key, profile)
			if err = s.Keyring.Remove(session.Key); err != nil {
				return n, err
			}
			n++
		}
	}

	return
}
