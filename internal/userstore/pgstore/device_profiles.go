package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// Compile-time interface check.
var _ userstore.DeviceProfileRegistry = (*PostgresUserStore)(nil)

// GetDeviceProfile returns the stored capability profile for one
// (profile, device), or nil when none has been reported.
func (s *PostgresUserStore) GetDeviceProfile(ctx context.Context, profileID, deviceID string) (*userstore.DeviceCapabilityProfile, error) {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(deviceID) == "" {
		return nil, nil
	}
	var p userstore.DeviceCapabilityProfile
	err := s.pool.QueryRow(ctx,
		`SELECT profile_id, device_id, codecs_video, codecs_audio, containers,
		        max_resolution, hdr, dolby_vision, source, capability_fingerprint,
		        updated_at::text, last_reported_at::text
		 FROM user_device_profiles
		 WHERE user_id = $1 AND profile_id = $2 AND device_id = $3`,
		s.userID, profileID, deviceID,
	).Scan(
		&p.ProfileID, &p.DeviceID, &p.CodecsVideo, &p.CodecsAudio, &p.Containers,
		&p.MaxResolution, &p.HDR, &p.DolbyVision, &p.Source, &p.Fingerprint,
		&p.UpdatedAt, &p.LastReportedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting device profile for device %q: %w", deviceID, err)
	}
	return &p, nil
}

// PutDeviceProfile upserts the capability profile for one (profile, device).
// A fingerprint equal to the stored one leaves updated_at untouched so
// redundant reports do not churn the row.
func (s *PostgresUserStore) PutDeviceProfile(ctx context.Context, profile userstore.DeviceCapabilityProfile) error {
	if strings.TrimSpace(profile.ProfileID) == "" || strings.TrimSpace(profile.DeviceID) == "" {
		return nil
	}
	if strings.TrimSpace(profile.Source) == "" {
		profile.Source = "client"
	}
	if strings.TrimSpace(profile.Fingerprint) == "" {
		profile.Fingerprint = userstore.DeviceCapabilityFingerprint(profile)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_device_profiles
			(user_id, profile_id, device_id, codecs_video, codecs_audio, containers,
			 max_resolution, hdr, dolby_vision, source, capability_fingerprint, last_reported_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		 ON CONFLICT (user_id, profile_id, device_id) DO UPDATE SET
			codecs_video = excluded.codecs_video,
			codecs_audio = excluded.codecs_audio,
			containers = excluded.containers,
			max_resolution = excluded.max_resolution,
			hdr = excluded.hdr,
			dolby_vision = excluded.dolby_vision,
			source = excluded.source,
			capability_fingerprint = excluded.capability_fingerprint,
			updated_at = CASE
				WHEN user_device_profiles.capability_fingerprint = excluded.capability_fingerprint
				THEN user_device_profiles.updated_at
				ELSE NOW()
			END,
			last_reported_at = NOW()`,
		s.userID, profile.ProfileID, profile.DeviceID,
		profile.CodecsVideo, profile.CodecsAudio, profile.Containers,
		profile.MaxResolution, profile.HDR, profile.DolbyVision, profile.Source, profile.Fingerprint,
	)
	if err != nil {
		return fmt.Errorf("putting device profile for device %q: %w", profile.DeviceID, err)
	}
	return nil
}

// ForgetDeviceProfile removes one (profile, device) capability profile.
func (s *PostgresUserStore) ForgetDeviceProfile(ctx context.Context, profileID, deviceID string) error {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(deviceID) == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM user_device_profiles
		 WHERE user_id = $1 AND profile_id = $2 AND device_id = $3`,
		s.userID, profileID, deviceID,
	); err != nil {
		return fmt.Errorf("forgetting device profile %q: %w", deviceID, err)
	}
	return nil
}
