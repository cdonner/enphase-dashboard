package main

// Optional Google Gemini provider for AI suggestions (set GEMINI_API_KEY; a free tier
// is available, under which Google may use requests to improve its products). Uses the
// REST API directly. Unless GEMINI_MODEL is set, the newest "Flash" model the key can
// use is picked from the model list, so retired model names don't break it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta" // replaced in tests

var geminiPicked struct {
	sync.Mutex
	model string
}

func geminiEnabled() bool { return os.Getenv("GEMINI_API_KEY") != "" }

// geminiModelSetting is the model for display: the configured one, the one picked, or "auto".
func geminiModelSetting() string {
	if m := os.Getenv("GEMINI_MODEL"); m != "" {
		return m
	}
	geminiPicked.Lock()
	defer geminiPicked.Unlock()
	if geminiPicked.model != "" {
		return geminiPicked.model
	}
	return "newest Flash"
}

// Gemini's response schema is an OpenAPI subset: upper-case types, no additionalProperties.
var geminiSchema = map[string]any{
	"type": "OBJECT",
	"properties": map[string]any{
		"signatures": map[string]any{
			"type": "ARRAY",
			"items": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"key": map[string]any{"type": "STRING"},
					"guesses": map[string]any{
						"type": "ARRAY",
						"items": map[string]any{
							"type": "OBJECT",
							"properties": map[string]any{
								"name":       map[string]any{"type": "STRING"},
								"confidence": map[string]any{"type": "STRING", "format": "enum", "enum": []string{"high", "medium", "low"}},
								"reason":     map[string]any{"type": "STRING"},
							},
							"required": []string{"name", "confidence", "reason"},
						},
					},
				},
				"required": []string{"key", "guesses"},
			},
		},
	},
	"required": []string{"signatures"},
}

func geminiDo(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, geminiBaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		if res.StatusCode == 429 {
			return errors.New("Gemini rate limit reached (free tier); try again in a minute")
		}
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("Gemini API error %d: %.300s", res.StatusCode, msg)
	}
	return json.Unmarshal(b, out)
}

var flashModel = regexp.MustCompile(`^models/gemini-(\d+(?:\.\d+)?)-flash$`)

// geminiModel returns GEMINI_MODEL, or picks (once) the newest plain Flash model.
func geminiModel(ctx context.Context) (string, error) {
	if m := os.Getenv("GEMINI_MODEL"); m != "" {
		return strings.TrimPrefix(m, "models/"), nil
	}
	geminiPicked.Lock()
	defer geminiPicked.Unlock()
	if geminiPicked.model != "" {
		return geminiPicked.model, nil
	}
	var list struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := geminiDo(ctx, "GET", "/models?pageSize=1000", nil, &list); err != nil {
		return "", err
	}
	type cand struct {
		name    string
		version float64
	}
	var cands []cand
	for _, m := range list.Models {
		canGenerate := false
		for _, g := range m.Methods {
			canGenerate = canGenerate || g == "generateContent"
		}
		if mm := flashModel.FindStringSubmatch(m.Name); canGenerate && mm != nil {
			v, _ := strconv.ParseFloat(mm[1], 64)
			cands = append(cands, cand{strings.TrimPrefix(m.Name, "models/"), v})
		}
	}
	if len(cands) == 0 {
		return "", errors.New("no Gemini Flash model available to this key; set GEMINI_MODEL")
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].version > cands[j].version })
	geminiPicked.model = cands[0].name
	return geminiPicked.model, nil
}

// askGemini sends the request with a JSON response schema and returns the JSON text.
func askGemini(ctx context.Context, prompt string) (string, error) {
	model, err := geminiModel(ctx)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": aiSystem}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   geminiSchema,
		},
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := geminiDo(ctx, "POST", "/models/"+model+":generateContent", body, &resp); err != nil {
		return "", err
	}
	if resp.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("Gemini declined the request (%s)", resp.PromptFeedback.BlockReason)
	}
	if len(resp.Candidates) == 0 {
		return "", errors.New("Gemini returned no answer")
	}
	var text string
	for _, p := range resp.Candidates[0].Content.Parts {
		text += p.Text
	}
	if text == "" {
		return "", fmt.Errorf("Gemini returned no text (finish reason %s)", resp.Candidates[0].FinishReason)
	}
	return text, nil
}
