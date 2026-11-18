// Package ai holds the workspace's language model settings (Neuralwatt,
// through fantasy's OpenAI-compatible provider) and the jobs that use it.
// Today that is one job: giving notes a title.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
)

const (
	// DefaultBaseURL is Neuralwatt's OpenAI-compatible API.
	DefaultBaseURL = "https://api.neuralwatt.com/v1/"
	// DefaultModel is Neuralwatt's alias for its current cheap, fast model,
	// so it moves to newer models without a change here.
	DefaultModel = "nw-flash"

	settingKey = "ai.neuralwatt"
	sealCtx    = "setting:" + settingKey
)

// ErrNotConfigured means no API key has been set (or there's no master key
// to unseal it with).
var ErrNotConfigured = errors.New("no AI provider configured")

// Config is what is needed to call the model.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
}

// Status describes the settings without the key.
type Status struct {
	Configured bool
	Model      string
	KeyHint    string // e.g. "…a1f3"
	UpdatedAt  time.Time
	UpdatedBy  string
}

// Settings stores the provider settings, with the API key sealed.
type Settings struct {
	db      *state.DB
	box     *secrets.Box // nil: no master key, so no API key can be stored
	baseURL string
}

// NewSettings makes Settings. box may be nil.
func NewSettings(db *state.DB, box *secrets.Box) *Settings {
	return &Settings{db: db, box: box, baseURL: DefaultBaseURL}
}

// SetBaseURL points at another OpenAI-compatible API, for tests.
func (s *Settings) SetBaseURL(u string) { s.baseURL = u }

// CanStoreKey reports whether a master key is available to seal the API key.
func (s *Settings) CanStoreKey() bool { return s.box != nil }

type stored struct {
	Model string `json:"model"`
}

// Status returns the current settings, without the key.
func (s *Settings) Status() (Status, error) {
	st, err := s.db.Setting(settingKey)
	if errors.Is(err, state.ErrNotFound) {
		return Status{Model: DefaultModel}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var v stored
	_ = json.Unmarshal([]byte(st.Value), &v)
	if v.Model == "" {
		v.Model = DefaultModel
	}
	return Status{Configured: len(st.Secret) > 0, Model: v.Model, KeyHint: st.SecretHint, UpdatedAt: st.UpdatedAt, UpdatedBy: st.UpdatedBy}, nil
}

// Config returns what's needed to call the model, or ErrNotConfigured.
func (s *Settings) Config() (Config, error) {
	st, err := s.db.Setting(settingKey)
	if errors.Is(err, state.ErrNotFound) || (err == nil && len(st.Secret) == 0) || s.box == nil {
		return Config{}, ErrNotConfigured
	}
	if err != nil {
		return Config{}, err
	}
	key, err := s.box.Open(sealCtx, st.Secret)
	if err != nil {
		return Config{}, fmt.Errorf("unsealing the AI key: %w", err)
	}
	var v stored
	_ = json.Unmarshal([]byte(st.Value), &v)
	if v.Model == "" {
		v.Model = DefaultModel
	}
	return Config{BaseURL: s.baseURL, Model: v.Model, APIKey: string(key)}, nil
}

// Save stores the model and, if apiKey isn't empty, a new key, after
// checking them with a real call. An empty apiKey keeps the current one.
func (s *Settings) Save(ctx context.Context, apiKey, model, user string) error {
	if s.box == nil {
		return errors.New("there's no master key to store the API key with (see BRAIN_MASTER_KEY_FILE)")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	apiKey = strings.TrimSpace(apiKey)
	var sealed []byte
	hint := ""
	if apiKey == "" {
		cur, err := s.db.Setting(settingKey)
		if err != nil || len(cur.Secret) == 0 {
			return errors.New("enter an API key")
		}
		k, err := s.box.Open(sealCtx, cur.Secret)
		if err != nil {
			return fmt.Errorf("unsealing the current key: %w", err)
		}
		apiKey, sealed, hint = string(k), cur.Secret, cur.SecretHint
	} else {
		sealed, hint = s.box.Seal(sealCtx, []byte(apiKey)), keyHint(apiKey)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := Title(cctx, Config{BaseURL: s.baseURL, Model: model, APIKey: apiKey}, "Testing the connection from ktx: buy milk and eggs."); err != nil {
		return fmt.Errorf("the provider refused: %w", err)
	}
	v, _ := json.Marshal(stored{Model: model})
	return s.db.PutSetting(state.Setting{Key: settingKey, Value: string(v), Secret: sealed, SecretHint: hint, UpdatedBy: user})
}

// Remove deletes the settings; notes then get titles from their first line.
func (s *Settings) Remove() error { return s.db.DeleteSetting(settingKey) }

func keyHint(k string) string {
	if len(k) <= 8 {
		return "…"
	}
	return "…" + k[len(k)-4:]
}

const titlePrompt = `You name notes. Reply with only a short, specific title for the note: at most 8 words, ` +
	`in the note's language, no quotes, no trailing full stop, no "Title:" prefix.`

// maxInput bounds what is sent to the model; the start of a note is enough
// to name it.
const maxInput = 8000

// Title asks the model for a note's title.
func Title(ctx context.Context, cfg Config, text string) (string, error) {
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(cfg.BaseURL),
		openaicompat.WithAPIKey(cfg.APIKey),
		// No WithName: provider options are looked up by the provider's
		// name, and NewProviderOptions uses the default one.
	)
	if err != nil {
		return "", err
	}
	model, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return "", err
	}
	if len(text) > maxInput {
		text = strings.ToValidUTF8(text[:maxInput], "")
	}
	agent := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(titlePrompt),
		fantasy.WithMaxOutputTokens(60),
		fantasy.WithTemperature(0.2),
		fantasy.WithMaxRetries(1),
		// A title needs no thinking; it's much faster without.
		fantasy.WithProviderOptions(openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
			ReasoningEffort: openai.ReasoningEffortOption(openai.ReasoningEffortNone),
		})),
	)
	res, err := agent.Generate(ctx, fantasy.AgentCall{Prompt: text})
	if err != nil {
		return "", err
	}
	t := CleanTitle(res.Response.Content.Text())
	if t == "" {
		return "", errors.New("the model returned no title")
	}
	return t, nil
}

// CleanTitle makes a model's reply into a title: its first line, without
// quotes, markdown or a "Title:" label, at most 100 characters.
func CleanTitle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "#*> ")
		if l := strings.ToLower(line); strings.HasPrefix(l, "title:") {
			line = strings.TrimSpace(line[len("title:"):])
		}
		line = strings.Trim(line, "\"'“”‘’*`_ ")
		line = strings.TrimRight(line, ".")
		return truncate(line, 100)
	}
	return ""
}

// FallbackTitle names a note from its first line, for when no model is
// available.
func FallbackTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*->[] "))
		if line != "" {
			return truncate(line, 60)
		}
	}
	return "Untitled note"
}

// truncate cuts s to max runes, at a word boundary when there's one, adding
// "…".
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := max - 1
	for i := cut; i > max/2; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	return strings.TrimSpace(string(r[:cut])) + "…"
}
