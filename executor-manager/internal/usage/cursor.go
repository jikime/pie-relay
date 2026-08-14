package usage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

var ErrInvalidCursor = errors.New("invalid usage cursor")

type listCursor struct {
	Version    int       `json:"v"`
	OccurredAt time.Time `json:"at"`
	DatabaseID int64     `json:"id"`
}

func encodeListCursor(item ListItem) string {
	encoded, _ := json.Marshal(listCursor{Version: 1, OccurredAt: item.OccurredAt.UTC(), DatabaseID: item.DatabaseID})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeListCursor(raw string) (*listCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > 1024 {
		return nil, ErrInvalidCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var cursor listCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil || cursor.Version != 1 || cursor.OccurredAt.IsZero() || cursor.DatabaseID < 1 {
		return nil, ErrInvalidCursor
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidCursor
	}
	cursor.OccurredAt = cursor.OccurredAt.UTC()
	return &cursor, nil
}
