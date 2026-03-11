package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"strings"
)

func loadHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()

	file, err := os.Open(resultsFile)
	if os.IsNotExist(err) {
		history = []HistoryEntry{}
		return
	}
	if err != nil {
		log.Println("Error opening history file:", err)
		history = []HistoryEntry{}
		return
	}
	defer file.Close()

	var loaded []HistoryEntry
	if err := json.NewDecoder(file).Decode(&loaded); err != nil {
		if errors.Is(err, io.EOF) {
			history = []HistoryEntry{}
			return
		}
		log.Println("Error decoding history:", err)
		history = []HistoryEntry{}
		return
	}

	history = loaded
}

func appendHistoryEntry(entry HistoryEntry) error {
	historyMu.Lock()
	defer historyMu.Unlock()

	history = append(history, entry)
	if err := persistHistoryLocked(); err != nil {
		history = history[:len(history)-1]
		return err
	}
	return nil
}

func deleteHistoryEntry(id *string) error {
	historyMu.Lock()
	defer historyMu.Unlock()

	backup := append([]HistoryEntry(nil), history...)

	if id != nil {
		idToDelete := strings.TrimSpace(*id)
		found := false
		for i, entry := range history {
			if entry.ID == idToDelete {
				history = append(history[:i], history[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return errHistoryIDNotFound
		}
	} else {
		history = []HistoryEntry{}
	}

	if err := persistHistoryLocked(); err != nil {
		history = backup
		return err
	}
	return nil
}

func persistHistoryLocked() error {
	file, err := os.Create(resultsFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(history)
}

func getHistorySnapshot() []HistoryEntry {
	historyMu.RLock()
	defer historyMu.RUnlock()

	snapshot := make([]HistoryEntry, len(history))
	copy(snapshot, history)
	return snapshot
}

func getHistoryEntryByID(id string) (*HistoryEntry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errHistoryIDNotFound
	}

	historyMu.RLock()
	defer historyMu.RUnlock()
	for _, entry := range history {
		if entry.ID == id {
			copied := entry
			return &copied, nil
		}
	}
	return nil, errHistoryIDNotFound
}

func getLatestHistoryEntry(excludeID *string) *HistoryEntry {
	historyMu.RLock()
	defer historyMu.RUnlock()

	var excluded string
	if excludeID != nil {
		excluded = strings.TrimSpace(*excludeID)
	}

	var latest *HistoryEntry
	for _, entry := range history {
		if excluded != "" && entry.ID == excluded {
			continue
		}
		if latest == nil || entry.Timestamp.After(latest.Timestamp) {
			copied := entry
			latest = &copied
		}
	}
	return latest
}
