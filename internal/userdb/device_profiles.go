package userdb

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// GetDeviceProfile returns the stored capability profile for one
// (profile, device), or nil when none has been reported.
func GetDeviceProfile(db *sql.DB, profileID, deviceID string) (*userstore.DeviceCapabilityProfile, error) {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(deviceID) == "" {
		return nil, nil
	}
	var p userstore.DeviceCapabilityProfile
	var hdr int
	var dolbyVision int
	var codecsVideo, codecsAudio, containers string
	err := db.QueryRow(
		`SELECT profile_id, device_id, codecs_video, codecs_audio, containers,
		        max_resolution, hdr, dolby_vision, source, capability_fingerprint,
		        updated_at, last_reported_at
		 FROM user_device_profiles
		 WHERE profile_id = ? AND device_id = ?`,
		profileID, deviceID,
	).Scan(
		&p.ProfileID, &p.DeviceID, &codecsVideo, &codecsAudio, &containers,
		&p.MaxResolution, &hdr, &dolbyVision, &p.Source, &p.Fingerprint,
		&p.UpdatedAt, &p.LastReportedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting device profile for device %q: %w", deviceID, err)
	}
	p.HDR = hdr != 0
	p.DolbyVision = dolbyVision != 0
	p.CodecsVideo = splitCapabilityList(codecsVideo)
	p.CodecsAudio = splitCapabilityList(codecsAudio)
	p.Containers = splitCapabilityList(containers)
	return &p, nil
}

// PutDeviceProfile upserts the capability profile for one (profile, device).
func PutDeviceProfile(db *sql.DB, profile userstore.DeviceCapabilityProfile) error {
	if strings.TrimSpace(profile.ProfileID) == "" || strings.TrimSpace(profile.DeviceID) == "" {
		return nil
	}
	if strings.TrimSpace(profile.Source) == "" {
		profile.Source = "client"
	}
	if strings.TrimSpace(profile.Fingerprint) == "" {
		profile.Fingerprint = userstore.DeviceCapabilityFingerprint(profile)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	hdr := 0
	if profile.HDR {
		hdr = 1
	}
	dolbyVision := 0
	if profile.DolbyVision {
		dolbyVision = 1
	}
	_, err := db.Exec(
		`INSERT INTO user_device_profiles
			(profile_id, device_id, codecs_video, codecs_audio, containers,
			 max_resolution, hdr, dolby_vision, source, capability_fingerprint, updated_at, last_reported_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(profile_id, device_id) DO UPDATE SET
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
				ELSE excluded.updated_at
			END,
			last_reported_at = excluded.last_reported_at`,
		profile.ProfileID, profile.DeviceID,
		joinCapabilityList(profile.CodecsVideo), joinCapabilityList(profile.CodecsAudio),
		joinCapabilityList(profile.Containers),
		profile.MaxResolution, hdr, dolbyVision, profile.Source, profile.Fingerprint, now, now,
	)
	if err != nil {
		return fmt.Errorf("putting device profile for device %q: %w", profile.DeviceID, err)
	}
	return nil
}

// ForgetDeviceProfile removes one (profile, device) capability profile.
func ForgetDeviceProfile(db *sql.DB, profileID, deviceID string) error {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(deviceID) == "" {
		return nil
	}
	if _, err := db.Exec(
		`DELETE FROM user_device_profiles WHERE profile_id = ? AND device_id = ?`,
		profileID, deviceID,
	); err != nil {
		return fmt.Errorf("forgetting device profile %q: %w", deviceID, err)
	}
	return nil
}

// joinCapabilityList flattens a normalized capability list into the SQLite
// TEXT column (comma-separated; values are normalized so commas cannot appear).
func joinCapabilityList(values []string) string {
	return strings.Join(userstore.NormalizeCapabilityValues(values), ",")
}

// splitCapabilityList reverses joinCapabilityList.
func splitCapabilityList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return userstore.NormalizeCapabilityValues(strings.Split(raw, ","))
}
