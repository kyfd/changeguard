package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

const (
	migrationWitnessSchema        = "changeguard-migration-witness/v1"
	migrationWitnessMarkerContent = "changeguard-migration-witness-required/v1\n"
	maxMigrationWitnessEntries    = 100000
)

type migrationWitnessArtifactEntry struct {
	Key           string `json:"key"`
	ArtifactID    string `json:"artifact_id"`
	ContentSHA256 string `json:"content_sha256"`
}

type migrationWitnessChangeEntry struct {
	Key            string `json:"key"`
	SQLSHA256      string `json:"sql_sha256"`
	RollbackSHA256 string `json:"rollback_sha256"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

type migrationWitnessSnapshot struct {
	StateSHA256 string                          `json:"state_sha256"`
	Changes     []migrationWitnessChangeEntry   `json:"changes"`
	Artifacts   []migrationWitnessArtifactEntry `json:"artifacts"`
}

type migrationWitnessDocument struct {
	Schema        string                    `json:"schema"`
	Current       migrationWitnessSnapshot  `json:"current"`
	Previous      *migrationWitnessSnapshot `json:"previous,omitempty"`
	PayloadSHA256 string                    `json:"payload_sha256"`
}

type MigrationWitnessStatus struct {
	Enabled             bool
	Reconciliation      string
	RestoredChanges     int
	RestoredArtifacts   int
	InterruptedSaveUsed bool
}

func (s *Store) MigrationWitnessStatus() MigrationWitnessStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.migrationWitnessStatus
}

func migrationWitnessPaths(dataPath string) (string, string, error) {
	witnessPath := strings.TrimSpace(os.Getenv("DBGUARD_MIGRATION_WITNESS_FILE"))
	if witnessPath == "" {
		witnessPath = dataPath + ".rollback-witness.json"
	}
	witnessPath = filepath.Clean(witnessPath)
	dataPath = filepath.Clean(dataPath)
	if witnessPath == dataPath {
		return "", "", errors.New("migration witness path must differ from the data file")
	}
	markerPath := witnessPath + ".required"
	if markerPath == dataPath {
		return "", "", errors.New("migration witness marker path must differ from the data file")
	}
	return witnessPath, markerPath, nil
}

func loadMigrationWitnessForState(witnessPath, markerPath string, stateContent []byte) (migrationWitnessSnapshot, MigrationWitnessStatus, error) {
	status := MigrationWitnessStatus{Enabled: witnessPath != ""}
	if witnessPath == "" {
		return migrationWitnessSnapshot{}, status, nil
	}
	markerExists, err := validateMigrationWitnessMarker(markerPath)
	if err != nil {
		return migrationWitnessSnapshot{}, status, err
	}
	content, err := os.ReadFile(witnessPath)
	if errors.Is(err, os.ErrNotExist) {
		if markerExists {
			return migrationWitnessSnapshot{}, status, errors.New("migration witness is required but missing")
		}
		if len(stateContent) == 0 {
			status.Reconciliation = "initialized"
			return migrationWitnessSnapshot{}, status, nil
		}
		status.Reconciliation = "initialized"
		return migrationWitnessSnapshot{}, status, nil
	}
	if err != nil {
		return migrationWitnessSnapshot{}, status, fmt.Errorf("read migration witness: %w", err)
	}
	if len(stateContent) == 0 {
		return migrationWitnessSnapshot{}, status, errors.New("migration witness exists without a data file")
	}
	document, err := decodeMigrationWitness(content)
	if err != nil {
		return migrationWitnessSnapshot{}, status, err
	}
	stateSHA256 := sha256Bytes(stateContent)
	snapshot := document.Current
	switch {
	case stateSHA256 == document.Current.StateSHA256:
		status.Reconciliation = "current-state"
	case document.Previous != nil && stateSHA256 == document.Previous.StateSHA256:
		snapshot = *document.Previous
		status.Reconciliation = "interrupted-save-recovered"
		status.InterruptedSaveUsed = true
	default:
		status.Reconciliation = "external-state-rehydrated"
	}
	return snapshot, status, nil
}

func decodeMigrationWitness(content []byte) (migrationWitnessDocument, error) {
	var document migrationWitnessDocument
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return migrationWitnessDocument{}, fmt.Errorf("decode migration witness: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return migrationWitnessDocument{}, errors.New("migration witness contains trailing data")
	}
	if document.Schema != migrationWitnessSchema {
		return migrationWitnessDocument{}, fmt.Errorf("unsupported migration witness schema %q", document.Schema)
	}
	if err := validateMigrationWitnessSnapshot(document.Current); err != nil {
		return migrationWitnessDocument{}, fmt.Errorf("invalid current migration witness: %w", err)
	}
	if document.Previous != nil {
		if err := validateMigrationWitnessSnapshot(*document.Previous); err != nil {
			return migrationWitnessDocument{}, fmt.Errorf("invalid previous migration witness: %w", err)
		}
	}
	payloadSHA256 := document.PayloadSHA256
	document.PayloadSHA256 = ""
	expected, err := migrationWitnessPayloadSHA256(document)
	if err != nil {
		return migrationWitnessDocument{}, err
	}
	if !strings.EqualFold(payloadSHA256, expected) {
		return migrationWitnessDocument{}, errors.New("migration witness payload digest mismatch")
	}
	document.PayloadSHA256 = strings.ToLower(payloadSHA256)
	return document, nil
}

func validateMigrationWitnessSnapshot(snapshot migrationWitnessSnapshot) error {
	if !validSHA256(snapshot.StateSHA256) {
		return errors.New("invalid state digest")
	}
	if len(snapshot.Changes)+len(snapshot.Artifacts) > maxMigrationWitnessEntries {
		return errors.New("migration witness entry limit exceeded")
	}
	if !sort.SliceIsSorted(snapshot.Changes, func(i, j int) bool { return snapshot.Changes[i].Key < snapshot.Changes[j].Key }) {
		return errors.New("change entries are not sorted")
	}
	if !sort.SliceIsSorted(snapshot.Artifacts, func(i, j int) bool { return snapshot.Artifacts[i].Key < snapshot.Artifacts[j].Key }) {
		return errors.New("artifact entries are not sorted")
	}
	seen := make(map[string]struct{}, len(snapshot.Changes)+len(snapshot.Artifacts))
	for _, entry := range snapshot.Changes {
		if !validSHA256(entry.Key) || !validSHA256(entry.SQLSHA256) || !validSHA256(entry.RollbackSHA256) || !validSHA256(entry.ArtifactSHA256) {
			return errors.New("invalid change witness entry")
		}
		key := "change|" + entry.Key
		if _, exists := seen[key]; exists {
			return errors.New("duplicate change witness entry")
		}
		seen[key] = struct{}{}
	}
	for _, entry := range snapshot.Artifacts {
		if !validSHA256(entry.Key) || !validSHA256(entry.ContentSHA256) || strings.TrimSpace(entry.ArtifactID) == "" || len(entry.ArtifactID) > 256 {
			return errors.New("invalid artifact witness entry")
		}
		key := "artifact|" + entry.Key
		if _, exists := seen[key]; exists {
			return errors.New("duplicate artifact witness entry")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func migrationWitnessPayloadSHA256(document migrationWitnessDocument) (string, error) {
	digest := sha256.New()
	writeField := func(value string) {
		_, _ = fmt.Fprintf(digest, "%d:", len([]byte(value)))
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{'\n'})
	}
	writeSnapshot := func(label string, snapshot migrationWitnessSnapshot) {
		writeField(label)
		writeField(snapshot.StateSHA256)
		writeField(strconv.Itoa(len(snapshot.Changes)))
		for _, entry := range snapshot.Changes {
			writeField(entry.Key)
			writeField(entry.SQLSHA256)
			writeField(entry.RollbackSHA256)
			writeField(entry.ArtifactSHA256)
		}
		writeField(strconv.Itoa(len(snapshot.Artifacts)))
		for _, entry := range snapshot.Artifacts {
			writeField(entry.Key)
			writeField(entry.ArtifactID)
			writeField(entry.ContentSHA256)
		}
	}
	writeField(document.Schema)
	writeSnapshot("current", document.Current)
	if document.Previous == nil {
		writeField("previous:none")
	} else {
		writeSnapshot("previous", *document.Previous)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func rehydrateMigrationWitness(data *state, snapshot migrationWitnessSnapshot) (int, int) {
	changes := make(map[string]migrationWitnessChangeEntry, len(snapshot.Changes))
	for _, entry := range snapshot.Changes {
		changes[entry.Key] = entry
	}
	artifacts := make(map[string]migrationWitnessArtifactEntry, len(snapshot.Artifacts))
	for _, entry := range snapshot.Artifacts {
		artifacts[entry.Key] = entry
	}
	restoredChanges := 0
	restoredArtifacts := 0
	for changeIndex := range data.Changes {
		change := &data.Changes[changeIndex]
		if entry, ok := changes[migrationWitnessChangeKey(*change)]; ok {
			changed := change.SQLSHA256 != entry.SQLSHA256 || change.RollbackSHA256 != entry.RollbackSHA256 || change.ArtifactSHA256 != entry.ArtifactSHA256
			change.SQLSHA256 = entry.SQLSHA256
			change.RollbackSHA256 = entry.RollbackSHA256
			change.ArtifactSHA256 = entry.ArtifactSHA256
			if changed {
				restoredChanges++
			}
		}
		keys := migrationWitnessArtifactKeys(*change)
		for artifactIndex := range change.Artifacts {
			entry, ok := artifacts[keys[artifactIndex]]
			if !ok {
				continue
			}
			artifact := &change.Artifacts[artifactIndex]
			changed := artifact.ID != entry.ArtifactID || !strings.EqualFold(artifact.ContentSHA256, entry.ContentSHA256)
			artifact.ID = entry.ArtifactID
			artifact.ContentSHA256 = entry.ContentSHA256
			if changed {
				restoredArtifacts++
			}
		}
	}
	return restoredChanges, restoredArtifacts
}

func buildMigrationWitnessSnapshot(data state, stateContent []byte) (migrationWitnessSnapshot, error) {
	snapshot := migrationWitnessSnapshot{StateSHA256: sha256Bytes(stateContent)}
	for _, change := range data.Changes {
		changeEntry := migrationWitnessChangeEntry{
			Key:            migrationWitnessChangeKey(change),
			SQLSHA256:      strings.ToLower(strings.TrimSpace(change.SQLSHA256)),
			RollbackSHA256: strings.ToLower(strings.TrimSpace(change.RollbackSHA256)),
			ArtifactSHA256: strings.ToLower(strings.TrimSpace(change.ArtifactSHA256)),
		}
		if !validSHA256(changeEntry.SQLSHA256) || !validSHA256(changeEntry.RollbackSHA256) || !validSHA256(changeEntry.ArtifactSHA256) {
			return migrationWitnessSnapshot{}, fmt.Errorf("change %q has incomplete integrity evidence", change.ID)
		}
		snapshot.Changes = append(snapshot.Changes, changeEntry)
		keys := migrationWitnessArtifactKeys(change)
		for artifactIndex, artifact := range change.Artifacts {
			entry := migrationWitnessArtifactEntry{
				Key:           keys[artifactIndex],
				ArtifactID:    strings.TrimSpace(artifact.ID),
				ContentSHA256: strings.ToLower(strings.TrimSpace(artifact.ContentSHA256)),
			}
			if entry.ArtifactID == "" || !validSHA256(entry.ContentSHA256) {
				return migrationWitnessSnapshot{}, fmt.Errorf("change %q artifact %d has incomplete integrity evidence", change.ID, artifactIndex)
			}
			snapshot.Artifacts = append(snapshot.Artifacts, entry)
		}
	}
	sort.Slice(snapshot.Changes, func(i, j int) bool { return snapshot.Changes[i].Key < snapshot.Changes[j].Key })
	sort.Slice(snapshot.Artifacts, func(i, j int) bool { return snapshot.Artifacts[i].Key < snapshot.Artifacts[j].Key })
	if err := validateMigrationWitnessSnapshot(snapshot); err != nil {
		return migrationWitnessSnapshot{}, err
	}
	return snapshot, nil
}

func persistMigrationWitness(witnessPath, markerPath string, current, previous migrationWitnessSnapshot) error {
	document := migrationWitnessDocument{Schema: migrationWitnessSchema, Current: current}
	if previous.StateSHA256 != "" && previous.StateSHA256 != current.StateSHA256 {
		copyOfPrevious := previous
		document.Previous = &copyOfPrevious
	}
	payloadSHA256, err := migrationWitnessPayloadSHA256(document)
	if err != nil {
		return err
	}
	document.PayloadSHA256 = payloadSHA256
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode migration witness: %w", err)
	}
	if err := writePrivateFileAtomic(witnessPath, content); err != nil {
		return fmt.Errorf("persist migration witness: %w", err)
	}
	if err := ensureMigrationWitnessMarker(markerPath); err != nil {
		return err
	}
	return nil
}

func migrationWitnessChangeKey(change model.ChangeRequest) string {
	parts := []string{
		strings.TrimSpace(change.ID),
		strings.TrimSpace(change.Environment),
		strings.TrimSpace(change.ChangeType),
		changegate.SHA256(change.SQL),
		changegate.SHA256(change.RollbackSQL),
		changegate.SHA256(change.RollbackPlan),
	}
	return changegate.SHA256(strings.Join(parts, "\x00"))
}

func migrationWitnessArtifactKeys(change model.ChangeRequest) []string {
	keys := make([]string, len(change.Artifacts))
	occurrences := make(map[string]int, len(change.Artifacts))
	for index, artifact := range change.Artifacts {
		parts := []string{
			strings.TrimSpace(change.ID),
			string(artifact.Kind),
			strings.TrimSpace(artifact.Name),
			strings.TrimSpace(artifact.Source),
			strings.TrimSpace(artifact.Language),
			changegate.SHA256(artifact.Content),
		}
		base := changegate.SHA256(strings.Join(parts, "\x00"))
		occurrence := occurrences[base]
		occurrences[base] = occurrence + 1
		keys[index] = changegate.SHA256(base + "\x00" + strconv.Itoa(occurrence))
	}
	return keys
}

func deterministicArtifactID(change model.ChangeRequest, artifact model.ChangeArtifact, occurrence int) string {
	parts := []string{
		strings.TrimSpace(change.ID),
		string(artifact.Kind),
		strings.TrimSpace(artifact.Name),
		strings.TrimSpace(artifact.Source),
		strings.TrimSpace(artifact.Language),
		changegate.SHA256(artifact.Content),
		strconv.Itoa(occurrence),
	}
	return "artifact_migrated_" + changegate.SHA256(strings.Join(parts, "\x00"))[:24]
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateMigrationWitnessMarker(path string) (bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read migration witness marker: %w", err)
	}
	if string(content) != migrationWitnessMarkerContent {
		return false, errors.New("migration witness marker is invalid")
	}
	return true, nil
}

func ensureMigrationWitnessMarker(path string) error {
	exists, err := validateMigrationWitnessMarker(path)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := writePrivateFileAtomic(path, []byte(migrationWitnessMarkerContent)); err != nil {
		return fmt.Errorf("persist migration witness marker: %w", err)
	}
	return nil
}

func writePrivateFileAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := file.Write(content); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	if err := file.Close(); writeErr == nil && err != nil {
		writeErr = err
	}
	if writeErr != nil {
		_ = os.Remove(temporary)
		return writeErr
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func sha256Bytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
