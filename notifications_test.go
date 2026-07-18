package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotificationStoreEncryptsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	secret := []byte(strings.Repeat("s", 32))
	store, err := newNotificationStore(path, secret)
	if err != nil {
		t.Fatal(err)
	}
	telegramEnabled := true
	telegramToken := "123456:telegram-secret"
	telegramChatID := "-100123456"
	emailEnabled := true
	smtpHost := "smtp.example.com"
	smtpPort := 465
	smtpUsername := "sender@example.com"
	smtpPassword := "smtp-secret"
	smtpFrom := "sender@example.com"
	smtpTo := "receiver@example.com"
	smtpSecurity := "auto"
	if err := store.update(notificationInput{
		TelegramEnabled:  &telegramEnabled,
		TelegramBotToken: &telegramToken,
		TelegramChatID:   &telegramChatID,
		EmailEnabled:     &emailEnabled,
		SMTPHost:         &smtpHost,
		SMTPPort:         &smtpPort,
		SMTPUsername:     &smtpUsername,
		SMTPPassword:     &smtpPassword,
		SMTPFrom:         &smtpFrom,
		SMTPTo:           &smtpTo,
		SMTPSecurity:     &smtpSecurity,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), telegramToken) || strings.Contains(string(raw), smtpPassword) {
		t.Fatal("notification settings contain plaintext secrets")
	}
	reloaded, err := newNotificationStore(path, secret)
	if err != nil {
		t.Fatal(err)
	}
	config := reloaded.get()
	if config.TelegramBotToken != telegramToken || config.SMTPPassword != smtpPassword {
		t.Fatal("encrypted notification secrets did not round-trip")
	}
	response := reloaded.response()
	if !response.TelegramBotTokenSet || !response.SMTPPasswordSet {
		t.Fatal("secret presence flags are missing")
	}

	blank := ""
	if err := reloaded.update(notificationInput{TelegramBotToken: &blank, SMTPPassword: &blank}); err != nil {
		t.Fatal(err)
	}
	if reloaded.get().TelegramBotToken != telegramToken || reloaded.get().SMTPPassword != smtpPassword {
		t.Fatal("blank secret input should preserve stored secrets")
	}

	disabled := false
	if err := reloaded.update(notificationInput{
		TelegramEnabled:       &disabled,
		EmailEnabled:          &disabled,
		ClearTelegramBotToken: true,
		ClearSMTPPassword:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if reloaded.get().TelegramBotToken != "" || reloaded.get().SMTPPassword != "" {
		t.Fatal("notification secrets were not cleared")
	}
}

func TestCollectHealthChanges(t *testing.T) {
	items := []bookmark{
		{ID: "recovered", Title: "Recovered"},
		{ID: "failed", Title: "Failed"},
		{ID: "new-failed", Title: "New failed"},
		{ID: "still-failed", Title: "Still failed"},
		{ID: "new-online", Title: "New online"},
	}
	previous := map[string]siteHealth{
		"recovered":    {ID: "recovered", Status: "down"},
		"failed":       {ID: "failed", Status: "online"},
		"still-failed": {ID: "still-failed", Status: "down"},
	}
	current := map[string]siteHealth{
		"recovered":    {ID: "recovered", Status: "online"},
		"failed":       {ID: "failed", Status: "down"},
		"new-failed":   {ID: "new-failed", Status: "down"},
		"still-failed": {ID: "still-failed", Status: "down"},
		"new-online":   {ID: "new-online", Status: "online"},
	}
	changes := collectHealthChanges(items, previous, current)
	if len(changes) != 3 {
		t.Fatalf("changes=%d, want 3", len(changes))
	}
	if !changes[0].Recovered || changes[1].Recovered || changes[2].Recovered {
		t.Fatalf("unexpected change classification: %#v", changes)
	}
}

func TestTelegramTestNotification(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123456:test-token/sendMessage" {
			t.Errorf("unexpected Telegram path: %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	store, err := newNotificationStore(filepath.Join(t.TempDir(), "notifications.json"), []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token := "123456:test-token"
	chatID := "-100123"
	if err := store.update(notificationInput{TelegramBotToken: &token, TelegramChatID: &chatID}); err != nil {
		t.Fatal(err)
	}
	service := newNotificationService(store)
	service.telegramAPIBase = server.URL
	service.httpClient = server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.sendTest(ctx, "telegram", "测试面板", "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-requests:
		if payload["chat_id"] != chatID || !strings.Contains(payload["text"].(string), "通知测试成功") {
			t.Fatalf("unexpected Telegram payload: %#v", payload)
		}
	default:
		t.Fatal("Telegram request was not received")
	}
}

func TestNotificationValidation(t *testing.T) {
	config := defaultNotificationConfig()
	config.IntervalMinutes = 0
	if err := validateNotificationConfig(config); err == nil {
		t.Fatal("expected zero monitoring interval rejected")
	}
	config = defaultNotificationConfig()
	config.TelegramEnabled = true
	if err := validateNotificationConfig(config); err == nil {
		t.Fatal("expected incomplete Telegram settings rejected")
	}
	config = defaultNotificationConfig()
	config.EmailEnabled = true
	if err := validateNotificationConfig(config); err == nil {
		t.Fatal("expected incomplete email settings rejected")
	}
}
