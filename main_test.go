package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBookmark(t *testing.T) {
	valid, err := validateBookmark(bookmarkInput{
		Title: " Example ",
		URL:   "https://example.com/path",
	})
	if err != nil {
		t.Fatalf("expected valid bookmark: %v", err)
	}
	if valid.Title != "Example" || valid.Category != "未分类" {
		t.Fatalf("unexpected normalization: %#v", valid)
	}

	invalidURLs := []string{"javascript:alert(1)", "file:///etc/passwd", "example.com"}
	for _, value := range invalidURLs {
		if _, err := validateBookmark(bookmarkInput{Title: "Bad", URL: value}); err == nil {
			t.Fatalf("expected URL to be rejected: %s", value)
		}
	}
}

func TestValidateBackground(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 20, G: 120, B: 80, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	if err := validateBackground(encoded.Bytes()); err != nil {
		t.Fatalf("expected PNG accepted: %v", err)
	}
	if err := validateBackground([]byte("not an image")); err == nil {
		t.Fatal("expected non-image rejected")
	}
}

func TestValidateSiteIcon(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 20, G: 120, B: 80, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	if contentType, err := validateSiteIcon(encoded.Bytes()); err != nil || contentType != "image/png" {
		t.Fatalf("expected PNG icon accepted, type=%q err=%v", contentType, err)
	}
	ico := make([]byte, 22+encoded.Len())
	copy(ico, []byte{0, 0, 1, 0, 1, 0, 32, 32, 0, 0, 1, 0, 32, 0})
	binary.LittleEndian.PutUint32(ico[14:18], uint32(encoded.Len()))
	binary.LittleEndian.PutUint32(ico[18:22], 22)
	copy(ico[22:], encoded.Bytes())
	if contentType, err := validateSiteIcon(ico); err != nil || contentType != "image/x-icon" {
		t.Fatalf("expected ICO icon accepted, type=%q err=%v", contentType, err)
	}
	if _, err := validateSiteIcon([]byte("not an icon")); err == nil {
		t.Fatal("expected non-image icon rejected")
	}
}

func TestDetectBookmarkIcon(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/favicon.ico" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(encoded.Bytes())
	}))
	defer server.Close()

	application := &app{health: newHealthManager(true)}
	raw, err := application.detectBookmarkIcon(context.Background(), bookmark{URL: server.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	if contentType, err := validateSiteIcon(raw); err != nil || contentType != "image/png" {
		t.Fatalf("unexpected detected icon: type=%q err=%v", contentType, err)
	}
}

func TestBlockedTargetIP(t *testing.T) {
	cases := []struct {
		address string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"8.8.8.8", false},
	}
	for _, test := range cases {
		if got := isBlockedTargetIP(net.ParseIP(test.address)); got != test.blocked {
			t.Fatalf("isBlockedTargetIP(%s)=%v, want %v", test.address, got, test.blocked)
		}
	}
}

func TestHealthChecker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	allowed := newHealthChecker(true).check(bookmark{ID: "allowed", URL: server.URL})
	if allowed.Status != "online" || allowed.HTTPCode != http.StatusNoContent {
		t.Fatalf("expected local test server online, got %#v", allowed)
	}
	blocked := newHealthChecker(false).check(bookmark{ID: "blocked", URL: server.URL})
	if blocked.Status != "down" || blocked.Error != "私有或保留地址已被保护" {
		t.Fatalf("expected private target blocked, got %#v", blocked)
	}
}

func TestStorePersistsAndDeduplicatesImports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.json")
	s, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.create(bookmarkInput{Title: "One", URL: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	added, err := s.importMany([]bookmarkInput{
		{Title: "Duplicate", URL: "https://example.com"},
		{Title: "Two", URL: "https://example.org", Category: "工作"},
	}, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || len(s.list()) != 2 {
		t.Fatalf("expected one imported bookmark, added=%d total=%d", added, len(s.list()))
	}

	reloaded, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.list()) != 2 {
		t.Fatalf("expected persisted bookmarks, got %d", len(reloaded.list()))
	}
}

func TestPreferencesPersistSiteName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	preferences, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := preferences.get().SiteName; got != defaultSiteName {
		t.Fatalf("default site name=%q, want %q", got, defaultSiteName)
	}
	if settings := preferences.get(); !settings.GlassEnabled || settings.GlassColor != defaultGlassColor {
		t.Fatalf("unexpected default glass settings: %#v", settings)
	}
	if settings := preferences.get(); settings.SiteSubtitle != defaultSiteSubtitle || settings.TimeZone != defaultTimeZone || settings.Note != "" {
		t.Fatalf("unexpected default personal settings: %#v", settings)
	}
	if err := preferences.updateSiteName("  我的导航站  "); err != nil {
		t.Fatal(err)
	}
	disabled := false
	color := "#38bdf8"
	subtitle := "我的私人入口"
	timeZone := "Asia/Tokyo"
	note := "购买域名\n检查备份"
	if err := preferences.update(preferencesInput{SiteSubtitle: &subtitle, TimeZone: &timeZone, Note: &note, GlassEnabled: &disabled, GlassColor: &color}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.get().SiteName; got != "我的导航站" {
		t.Fatalf("persisted site name=%q", got)
	}
	if settings := reloaded.get(); settings.GlassEnabled || settings.GlassColor != color {
		t.Fatalf("unexpected persisted glass settings: %#v", settings)
	}
	if settings := reloaded.get(); settings.SiteSubtitle != subtitle || settings.TimeZone != timeZone || settings.Note != note {
		t.Fatalf("unexpected persisted personal settings: %#v", settings)
	}
	if err := reloaded.updateSiteName(strings.Repeat("签", maxSiteNameRunes+1)); err == nil {
		t.Fatal("expected overlong site name rejected")
	}
	if err := reloaded.updateSiteName("第一行\n第二行"); err == nil {
		t.Fatal("expected multiline site name rejected")
	}
	invalidColor := "blue"
	if err := reloaded.update(preferencesInput{GlassColor: &invalidColor}); err == nil {
		t.Fatal("expected invalid glass color rejected")
	}
	invalidTimeZone := "Mars/Olympus"
	if err := reloaded.update(preferencesInput{TimeZone: &invalidTimeZone}); err == nil {
		t.Fatal("expected invalid time zone rejected")
	}
	overlongSubtitle := strings.Repeat("签", maxSiteSubtitleRunes+1)
	if err := reloaded.update(preferencesInput{SiteSubtitle: &overlongSubtitle}); err == nil {
		t.Fatal("expected overlong subtitle rejected")
	}
	overlongNote := strings.Repeat("记", maxNoteRunes+1)
	if err := reloaded.update(preferencesInput{Note: &overlongNote}); err == nil {
		t.Fatal("expected overlong note rejected")
	}
}

func TestCredentialStoreChangesPasswordAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	credentials, err := newCredentialStore(path, "original-password")
	if err != nil {
		t.Fatal(err)
	}
	if !credentials.verify("original-password") || credentials.verify("wrong-password") {
		t.Fatal("unexpected initial password verification result")
	}
	initialVersion := credentials.sessionVersion()
	if err := credentials.change("wrong-password", "replacement-password"); !errors.Is(err, errCurrentPassword) {
		t.Fatalf("expected current password error, got %v", err)
	}
	if err := credentials.change("original-password", "replacement-password"); err != nil {
		t.Fatal(err)
	}
	if credentials.verify("original-password") || !credentials.verify("replacement-password") {
		t.Fatal("password was not replaced")
	}
	if credentials.sessionVersion() != initialVersion+1 {
		t.Fatal("password change did not invalidate existing sessions")
	}

	reloaded, err := newCredentialStore(path, "ignored-bootstrap-password")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.verify("replacement-password") || reloaded.sessionVersion() != initialVersion+1 {
		t.Fatal("changed password did not persist")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("replacement-password")) {
		t.Fatal("credentials file contains the plaintext password")
	}
}

func TestDefaultInitialPassword(t *testing.T) {
	credentials, err := newCredentialStore(filepath.Join(t.TempDir(), "credentials.json"), defaultInitialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !credentials.verify(defaultInitialPassword) {
		t.Fatal("default initial password was not accepted")
	}
	if err := validateInitialPassword("123456"); err == nil {
		t.Fatal("unexpectedly accepted a non-default short initial password")
	}
}
