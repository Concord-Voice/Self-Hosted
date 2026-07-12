package presencehistory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// Category is a database taxonomy value. A valid category is not, by itself,
// authorization to persist a payload; writes remain exposed only through typed
// category-specific repository methods.
type Category string

// Supported Activity History storage taxonomy values.
const (
	CategoryServerVoice  Category = "server_voice"
	CategoryPrivateCall  Category = "private_call"
	CategoryGames        Category = "games"
	CategoryMusic        Category = "music"
	CategoryStreaming    Category = "streaming"
	CategoryBrowser      Category = "browser"
	CategoryProductivity Category = "productivity"
	CategoryCreator      Category = "creator"
	CategoryCustomText   Category = "custom_text"
)

var allCategories = []Category{
	CategoryServerVoice,
	CategoryPrivateCall,
	CategoryGames,
	CategoryMusic,
	CategoryStreaming,
	CategoryBrowser,
	CategoryProductivity,
	CategoryCreator,
	CategoryCustomText,
}

// Valid reports whether the category belongs to the storage taxonomy.
func (c Category) Valid() bool {
	switch c {
	case CategoryServerVoice,
		CategoryPrivateCall,
		CategoryGames,
		CategoryMusic,
		CategoryStreaming,
		CategoryBrowser,
		CategoryProductivity,
		CategoryCreator,
		CategoryCustomText:
		return true
	default:
		return false
	}
}

// CustomTextState is the v1 typed payload for a Custom Status interval.
type CustomTextState struct {
	Text  string `json:"text"`
	Emoji string `json:"emoji,omitempty"`
}

type payloadKey struct {
	Category Category
	Version  int16
}

var payloadReaders = map[payloadKey]func(json.RawMessage) (any, error){
	{Category: CategoryCustomText, Version: 1}: decodeCustomTextV1,
}

func decodeCustomTextV1(raw json.RawMessage) (any, error) {
	fields, err := decodeExactJSONObject(raw, "text", "emoji")
	if err != nil {
		return nil, fmt.Errorf("decode custom text v1: %w", err)
	}

	var text *string
	if rawText, ok := fields["text"]; !ok || json.Unmarshal(rawText, &text) != nil || text == nil {
		return nil, fmt.Errorf("decode custom text v1: text must be a string")
	}
	state := CustomTextState{Text: *text}
	if rawEmoji, ok := fields["emoji"]; ok {
		var emoji *string
		if err := json.Unmarshal(rawEmoji, &emoji); err != nil || emoji == nil {
			return nil, fmt.Errorf("decode custom text v1: emoji must be a string")
		}
		state.Emoji = *emoji
	}

	textLength := utf8.RuneCountInString(state.Text)
	if textLength < 1 || textLength > 140 {
		return nil, fmt.Errorf("custom text v1 text must contain 1 to 140 code points")
	}
	if utf8.RuneCountInString(state.Emoji) > 32 {
		return nil, fmt.Errorf("custom text v1 emoji must contain at most 32 code points")
	}
	return state, nil
}

func decodeExactJSONObject(raw []byte, allowedKeys ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = struct{}{}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}

	fields := make(map[string]json.RawMessage, len(allowedKeys))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key")
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}

	token, err = decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok = token.(json.Delim)
	if !ok || delimiter != '}' {
		return nil, fmt.Errorf("expected end of JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing JSON")
}
