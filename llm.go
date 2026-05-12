package main

import (
	_ "embed"

	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/ini.v1"
)

// defaults.ini, prompts.ini, and secrets.ini are embedded into the
// binary so a single-file deploy of mrgraise.exe works without these
// files present. A file of the same name in the exe directory at
// runtime takes precedence — drop one next to the exe to override
// per-machine.
//
// SECURITY NOTE: the embedded secrets.ini bakes the API key into the
// binary in plaintext. Anyone who can run `strings` on the .exe (or
// distribute it) can extract the key. This is an explicit trade-off
// for single-file deployment; treat the .exe itself as sensitive.
//
//go:embed defaults.ini
var embeddedDefaultsIni []byte

//go:embed prompts.ini
var embeddedPromptsIni []byte

//go:embed secrets.ini
var embeddedSecretsIni []byte

var phrasesToFilter = []string{
	"EXAM:", "EXAM DATE:", "TECHNIQUE:", "COMPARISON:", "IV Contrast:",
	"Oral Contrast:", "RADIATION DOSE:", "If you are a provider",
	"RADIOPHARMACEUTICAL:", "FINGERSTICK", "UPTAKE TIME:", "LIMITATIONS:",
	"Reference", "Comment:",
}

// Config holds the application configuration loaded from ini files.
type Config struct {
	Model       string
	Temperature float64
	MaxTokens   int
	APIKey      string
	// Prompts
	SystemPrompt       string
	CheckReportQuery   string
	GenImpressionQuery string
}

// APIMessage represents a single message in the API request.
type APIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// APIRequest is the request body for the Anthropic messages API.
type APIRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature float64      `json:"temperature"`
	System      string       `json:"system"`
	Messages    []APIMessage `json:"messages"`
}

// ContentBlock represents a content block in the API response.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// APIResponse is the response body from the Anthropic messages API.
type APIResponse struct {
	Content []ContentBlock `json:"content"`
}

// APIError represents an error response from the Anthropic API.
type APIError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// loadIniDiskOrEmbedded reads filename from the exe directory if it
// exists, otherwise falls back to the supplied embedded bytes. Returns
// an error only if neither source is loadable (i.e., disk file is
// missing AND there's no embedded fallback).
func loadIniDiskOrEmbedded(filename string, embedded []byte) (*ini.File, error) {
	diskPath := filepath.Join(getExeDir(), filename)
	if data, err := os.ReadFile(diskPath); err == nil {
		return ini.Load(data)
	}
	if len(embedded) > 0 {
		return ini.Load(embedded)
	}
	return nil, fmt.Errorf("%s not found in %s and no embedded fallback", filename, getExeDir())
}

func loadConfig() Config {
	// Defaults for the model in case defaults.ini can't be read at all
	// (no disk file, no embedded fallback).
	cfg := Config{
		Model:       "claude-haiku-4-5-20251001",
		Temperature: 0.3,
		MaxTokens:   4096,
	}

	// Load defaults.ini — disk overrides embedded.
	if iniFile, err := loadIniDiskOrEmbedded("defaults.ini", embeddedDefaultsIni); err == nil {
		if key, err := iniFile.Section("model").GetKey("name"); err == nil {
			cfg.Model = key.String()
		}
		if key, err := iniFile.Section("api").GetKey("temperature"); err == nil {
			if v, err := key.Float64(); err == nil {
				cfg.Temperature = v
			}
		}
		if key, err := iniFile.Section("api").GetKey("max_tokens"); err == nil {
			if v, err := key.Int(); err == nil {
				cfg.MaxTokens = v
			}
		}
	}

	// Load API key from secrets.ini — disk overrides embedded.
	if iniFile, err := loadIniDiskOrEmbedded("secrets.ini", embeddedSecretsIni); err == nil {
		if key, err := iniFile.Section("api").GetKey("key"); err == nil {
			cfg.APIKey = key.String()
		}
	}

	// Load prompts.ini — disk overrides embedded. Required: assert if
	// the embedded fallback is also missing or malformed.
	promptsFile, err := loadIniDiskOrEmbedded("prompts.ini", embeddedPromptsIni)
	assert(err == nil, fmt.Sprintf("Failed to load prompts.ini: %v", err))
	promptsSec := promptsFile.Section("prompts")
	cfg.SystemPrompt = promptsSec.Key("system").String()
	cfg.CheckReportQuery = promptsSec.Key("check_report").String()
	cfg.GenImpressionQuery = promptsSec.Key("gen_impression").String()
	assert(cfg.SystemPrompt != "", "system prompt not found in prompts.ini")
	assert(cfg.CheckReportQuery != "", "check_report prompt not found in prompts.ini")
	assert(cfg.GenImpressionQuery != "", "gen_impression prompt not found in prompts.ini")

	assert(cfg.APIKey != "", "API key not found. Create a secrets.ini file with [api] key = your-api-key")
	return cfg
}

func getExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		// Fall back to working directory
		dir, _ := os.Getwd()
		return dir
	}
	return filepath.Dir(exe)
}

// LLM config is loaded the first time an LLM hotkey is invoked so users
// who don't have prompts.ini / secrets.ini set up can still use the
// non-LLM hotkeys without the program panicking at startup. Successful
// loads are cached; failures are NOT cached, so the user can fix a
// missing file and retry the hotkey without restarting.
var (
	llmCfgMu sync.Mutex
	llmCfg   Config
	llmCfgOK bool
)

func getLLMConfig() (Config, error) {
	llmCfgMu.Lock()
	defer llmCfgMu.Unlock()

	if llmCfgOK {
		return llmCfg, nil
	}

	var loadErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				loadErr = fmt.Errorf("%v", r)
			}
		}()
		llmCfg = loadConfig()
		llmCfgOK = true
	}()
	return llmCfg, loadErr
}

func queryClaude(cfg Config, userPrompt string, userInput string) (string, error) {
	assert(userPrompt != "", "userPrompt must not be empty")
	assert(userInput != "", "userInput must not be empty")

	reqBody := APIRequest{
		Model:       cfg.Model,
		MaxTokens:   cfg.MaxTokens,
		Temperature: cfg.Temperature,
		System:      cfg.SystemPrompt,
		Messages: []APIMessage{
			{Role: "user", Content: userPrompt},
			{Role: "user", Content: userInput},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr APIError
		if json.Unmarshal(body, &apiErr) == nil {
			return "", fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return "", fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("empty response from API")
	}

	return strings.TrimSpace(apiResp.Content[0].Text), nil
}

// runLLMQuery sends userInput to Claude with either the generate-impression
// or check-report prompt and returns the response text formatted for
// display. In impression mode the cleaned impression text is also placed
// on the clipboard so the user can paste it back into the report with
// Ctrl+V; a note about the clipboard outcome is appended to the returned
// display text.
func runLLMQuery(cfg Config, userInput string, generateImpression bool) (string, error) {
	filtered := filterParagraphs(userInput, phrasesToFilter)

	if generateImpression {
		result, err := queryClaude(cfg, cfg.GenImpressionQuery, filtered)
		if err != nil {
			return "", err
		}
		errs := extractErrorsBeforeImpression(result)
		paragraphs := numberParagraphs(stripMarkdown(removeNumbering(result)))
		clipboardNote := "(Impression has been placed in the clipboard. Press Ctrl-V to insert into report.)"
		if err := setClipboardText(paragraphs); err != nil {
			clipboardNote = fmt.Sprintf("(Clipboard error: %v)", err)
		}
		return fmt.Sprintf("ERRORS:\n%s\n\nIMPRESSION:\n%s\n\n%s", errs, paragraphs, clipboardNote), nil
	}

	result, err := queryClaude(cfg, cfg.CheckReportQuery, filtered)
	if err != nil {
		return "", err
	}
	return stripMarkdown(result), nil
}
