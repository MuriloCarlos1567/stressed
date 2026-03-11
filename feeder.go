package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

type feederDataset struct {
	rows    []map[string]string
	loop    bool
	counter uint64
}

func loadDataFeeder(cfg DataFeederConfig) (*feederDataset, error) {
	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	switch format {
	case dataFeederFormatCSV:
		rows, err := loadCSVRows(cfg.File)
		if err != nil {
			return nil, err
		}
		return &feederDataset{rows: rows, loop: cfg.Loop}, nil
	case dataFeederFormatJSON:
		rows, err := loadJSONRows(cfg.File)
		if err != nil {
			return nil, err
		}
		return &feederDataset{rows: rows, loop: cfg.Loop}, nil
	default:
		return nil, errors.New("unsupported data feeder format")
	}
}

func (f *feederDataset) NextRow() (map[string]string, bool) {
	if f == nil || len(f.rows) == 0 {
		return map[string]string{}, true
	}

	index := int(atomic.AddUint64(&f.counter, 1) - 1)
	if f.loop {
		row := f.rows[index%len(f.rows)]
		return copyStringMap(row), true
	}
	if index >= len(f.rows) {
		return nil, false
	}
	return copyStringMap(f.rows[index]), true
}

func loadCSVRows(path string) ([]map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed opening csv feeder: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed parsing csv feeder: %w", err)
	}
	if len(records) < 2 {
		return nil, errors.New("csv feeder must have header and at least one row")
	}

	headers := make([]string, len(records[0]))
	for i, header := range records[0] {
		headers[i] = strings.TrimSpace(header)
	}

	rows := make([]map[string]string, 0, len(records)-1)
	for rowIndex := 1; rowIndex < len(records); rowIndex++ {
		record := records[rowIndex]
		entry := make(map[string]string, len(headers))
		for i, header := range headers {
			if header == "" {
				continue
			}
			value := ""
			if i < len(record) {
				value = strings.TrimSpace(record[i])
			}
			entry[header] = value
		}
		rows = append(rows, entry)
	}
	return rows, nil
}

func loadJSONRows(path string) ([]map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed reading json feeder: %w", err)
	}

	var records []map[string]interface{}
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("failed parsing json feeder: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("json feeder must contain at least one object")
	}

	rows := make([]map[string]string, 0, len(records))
	for _, record := range records {
		entry := make(map[string]string, len(record))
		for key, value := range record {
			entry[strings.TrimSpace(key)] = valueToString(value)
		}
		rows = append(rows, entry)
	}
	return rows, nil
}

func copyStringMap(source map[string]string) map[string]string {
	copyMap := make(map[string]string, len(source))
	for key, value := range source {
		copyMap[key] = value
	}
	return copyMap
}
