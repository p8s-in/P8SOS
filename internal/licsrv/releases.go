package licsrv

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"p8sos/internal/license"
	"p8sos/internal/release"
)

// Channels: testing gets every release; stable only what was promoted.
const (
	Testing = "testing"
	Stable  = "stable"
)

// Release is a published update bundle.
type Release struct {
	release.Manifest
	Signature string   `json:"signature"`
	Channels  []string `json:"channels"`
	Published string   `json:"published"`
	Promoted  string   `json:"promoted,omitempty"`
}

func (r *Release) in(ch string) bool {
	for _, c := range r.Channels {
		if c == ch {
			return true
		}
	}
	return false
}

func (s *Store) releaseDir() string { return filepath.Join(s.Dir, "releases") }

// ReleaseFile is where a bundle is kept.
func (s *Store) ReleaseFile(version string) string {
	return filepath.Join(s.releaseDir(), "p8sos-"+version+".tar.gz")
}

// AddRelease bundles a source folder, signs it and publishes it on testing
// (and stable too when stable is set).
func (s *Store) AddRelease(dir, version, notes string, stable bool) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Release{}, err
	}
	for _, r := range s.st.Releases {
		if r.Version == version {
			return Release{}, fmt.Errorf("%w: release %s exists; pick a new version", ErrInvalid, version)
		}
	}
	if err := os.MkdirAll(s.releaseDir(), 0o700); err != nil {
		return Release{}, err
	}
	m, err := release.Bundle(dir, version, notes, s.ReleaseFile(version))
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	now := s.now().Format(time.RFC3339)
	r := &Release{Manifest: m, Signature: release.Sign(s.Key, m), Channels: []string{Testing}, Published: now}
	if stable {
		r.Channels, r.Promoted = append(r.Channels, Stable), now
	}
	s.st.Releases = append(s.st.Releases, r)
	return *r, s.save()
}

// Promote puts a tested release on the stable channel.
func (s *Store) Promote(version string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Release{}, err
	}
	for _, r := range s.st.Releases {
		if r.Version == version {
			if !r.in(Stable) {
				r.Channels, r.Promoted = append(r.Channels, Stable), s.now().Format(time.RFC3339)
			}
			return *r, s.save()
		}
	}
	return Release{}, fmt.Errorf("%w: release %s", ErrNotFound, version)
}

// Releases lists releases, newest first.
func (s *Store) Releases() ([]Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := []Release{}
	for i := len(s.st.Releases) - 1; i >= 0; i-- {
		out = append(out, *s.st.Releases[i])
	}
	return out, nil
}

// Latest is the newest release on a channel.
func (s *Store) Latest(channel string) (Release, bool) {
	l, err := s.Releases()
	if err != nil {
		return Release{}, false
	}
	for _, r := range l {
		if r.in(channel) {
			return r, true
		}
	}
	return Release{}, false
}

// release finds one release on a channel.
func (s *Store) releaseOn(version, channel string) (Release, bool) {
	l, _ := s.Releases()
	for _, r := range l {
		if r.Version == version && r.in(channel) {
			return r, true
		}
	}
	return Release{}, false
}

// MayUpdate says whether a server may use a channel: testing is open to all,
// stable needs a licence for that server whose tier includes stable updates.
func (s *Store) MayUpdate(channel, licenseID, server string) error {
	switch channel {
	case Testing:
		return nil
	case Stable:
	default:
		return fmt.Errorf("%w: channel %q", ErrInvalid, channel)
	}
	r, err := s.Get(licenseID)
	if err != nil || !r.License.Covers(server) || r.Revoked {
		return fmt.Errorf("%w: stable updates need an active subscription for this server", ErrSubscription)
	}
	if s.now().After(r.License.End().Add(license.Grace)) {
		return fmt.Errorf("%w: the subscription has expired; renew it to get stable updates", ErrSubscription)
	}
	if !license.Allows(r.License.Tier, license.FeatStable) {
		return fmt.Errorf("%w: this tier has no stable updates", ErrSubscription)
	}
	return nil
}
