package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAskGemini(t *testing.T) {
	var gotKey, gotModelPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/models"):
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
				{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.0-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.0-flash-lite", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.0-pro", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/text-embedding-004", "supportedGenerationMethods": []string{"embedContent"}},
			}})
		case r.Method == "POST":
			gotModelPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&gotBody)
			answer := `{"signatures":[{"key":"0.27","guesses":[{"name":"Coffee maker warming plate","confidence":"high","reason":"400 W resistive pulses every 16 s."}]}]}`
			json.NewEncoder(w).Encode(map[string]any{"candidates": []map[string]any{
				{"content": map[string]any{"parts": []map[string]string{{"text": answer}}}, "finishReason": "STOP"},
			}})
		}
	}))
	defer srv.Close()
	old := geminiBaseURL
	geminiBaseURL = srv.URL
	defer func() { geminiBaseURL = old }()
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("GEMINI_MODEL", "")
	geminiPicked.model, geminiPicked.candidates = "", nil

	s := NewServer(Config{DataDir: t.TempDir()})
	for d := 0; d < 2; d++ {
		start := startOfDay(time.Now().AddDate(0, 0, -d)).UnixMilli() + 8*3_600_000
		end := start + 3000
		s.writeJSON("events/"+dayKey(time.Now().AddDate(0, 0, -d))+".json",
			dayRuns{Runs: []Run{{Start: start, End: &end, DP: []float64{0, 400}, DQ: []float64{0, 4}}}}, 0o644)
	}
	res, err := s.askAI(t.Context(), "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "test-key" {
		t.Errorf("api key header %q", gotKey)
	}
	if gotModelPath != "/models/gemini-3.0-flash:generateContent" {
		t.Errorf("picked model path %q, want the newest plain Flash", gotModelPath)
	}
	gc, _ := gotBody["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseSchema"] == nil {
		t.Errorf("generationConfig %v", gc)
	}
	got := res["0.27"]
	if got.Source != "gemini" || len(got.Guesses) != 1 || got.Guesses[0].Confidence != "high" {
		t.Errorf("stored %+v", got)
	}
	if ps := aiProviders(); len(ps) == 0 || ps[len(ps)-1].ID != "gemini" || ps[len(ps)-1].Model != "gemini-3.0-flash" {
		t.Errorf("providers %+v", ps)
	}
	if _, err := s.askAI(t.Context(), "nope"); err == nil {
		t.Error("unknown provider should fail")
	}
}

func TestGeminiFallsBackWhenBusy(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
				{"name": "models/gemini-3.8-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.0-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-3.8-flash-lite", "supportedGenerationMethods": []string{"generateContent"}},
			}})
		default:
			model := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/models/"), ":generateContent")
			tried = append(tried, model)
			switch model {
			case "gemini-3.8-flash": // overloaded
				w.WriteHeader(503)
				w.Write([]byte(`{"error":{"code":503,"message":"high demand","status":"UNAVAILABLE"}}`))
			case "gemini-3.5-flash": // hangs
				time.Sleep(300 * time.Millisecond)
			default:
				json.NewEncoder(w).Encode(map[string]any{"candidates": []map[string]any{
					{"content": map[string]any{"parts": []map[string]string{{"text": `{"signatures":[]}`}}}},
				}})
			}
		}
	}))
	defer srv.Close()
	oldURL, oldTimeout := geminiBaseURL, geminiAttemptTimeout
	geminiBaseURL, geminiAttemptTimeout = srv.URL, 100*time.Millisecond
	defer func() { geminiBaseURL, geminiAttemptTimeout = oldURL, oldTimeout }()
	t.Setenv("GEMINI_API_KEY", "k")
	t.Setenv("GEMINI_MODEL", "")
	geminiPicked.model, geminiPicked.candidates = "", nil

	if _, err := askGemini(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tried, ",") != "gemini-3.8-flash,gemini-3.5-flash,gemini-3.0-flash" {
		t.Errorf("tried %v", tried)
	}
	if geminiPicked.model != "gemini-3.0-flash" {
		t.Errorf("answered by %q", geminiPicked.model)
	}
	// Flash-Lite comes last.
	if c := geminiPicked.candidates; c[len(c)-1] != "gemini-3.8-flash-lite" {
		t.Errorf("candidates %v", c)
	}
}
