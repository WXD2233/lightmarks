package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxBookmarks           = 2000
	maxRequestBytes        = 1 << 20
	maxDataBytes           = 4 << 20
	maxSettingsBytes       = 8 << 10
	sessionDuration        = 30 * 24 * time.Hour
	checkWorkers           = 6
	checkTimeout           = 6 * time.Second
	maxCheckDrainBytes     = 8 << 10
	maxBackgroundBytes     = 2 << 20
	maxBackgroundPixels    = 20_000_000
	defaultSiteName        = "轻签监控台"
	maxSiteNameRunes       = 24
	defaultSiteSubtitle    = "私人书签与网站状态监测"
	maxSiteSubtitleRunes   = 40
	defaultTimeZone        = "Asia/Shanghai"
	maxNoteRunes           = 1000
	defaultGlassColor      = "#ffffff"
	defaultInitialPassword = "123123"
	passwordIterations     = 600_000
	passwordSaltBytes      = 16
	passwordHashBytes      = 32
	maxIconBytes           = 256 << 10
	maxIconPixels          = 4_000_000
)

//go:embed web/*
var webFiles embed.FS

type bookmark struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Category    string    `json:"category"`
	Description string    `json:"description,omitempty"`
	HasIcon     bool      `json:"hasIcon,omitempty"`
	IconVersion int64     `json:"iconVersion,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type bookmarkInput struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

type database struct {
	Version   int        `json:"version"`
	Bookmarks []bookmark `json:"bookmarks"`
}

type sitePreferences struct {
	SiteName     string `json:"siteName"`
	SiteSubtitle string `json:"siteSubtitle"`
	TimeZone     string `json:"timeZone"`
	Note         string `json:"note,omitempty"`
	GlassEnabled bool   `json:"glassEnabled"`
	GlassColor   string `json:"glassColor"`
}

type preferencesInput struct {
	SiteName     *string `json:"siteName"`
	SiteSubtitle *string `json:"siteSubtitle"`
	TimeZone     *string `json:"timeZone"`
	Note         *string `json:"-"`
	GlassEnabled *bool   `json:"glassEnabled"`
	GlassColor   *string `json:"glassColor"`
}

type preferencesStore struct {
	mu   sync.RWMutex
	path string
	data sitePreferences
}

func newPreferencesStore(path string) (*preferencesStore, error) {
	p := &preferencesStore{
		path: path,
		data: sitePreferences{
			SiteName:     defaultSiteName,
			SiteSubtitle: defaultSiteSubtitle,
			TimeZone:     defaultTimeZone,
			GlassEnabled: true,
			GlassColor:   defaultGlassColor,
		},
	}
	if err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *preferencesStore) load() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	info, err := os.Stat(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect settings file: %w", err)
	}
	if info.Size() > maxSettingsBytes {
		return fmt.Errorf("settings file exceeds %d KiB safety limit", maxSettingsBytes>>10)
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("read settings file: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	settings := p.data
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("decode settings file: %w", err)
	}
	name, err := validateSiteName(settings.SiteName)
	if err != nil {
		return fmt.Errorf("invalid settings file: %w", err)
	}
	color, err := validateGlassColor(settings.GlassColor)
	if err != nil {
		return fmt.Errorf("invalid settings file: %w", err)
	}
	subtitle, err := validateSiteSubtitle(settings.SiteSubtitle)
	if err != nil {
		return fmt.Errorf("invalid settings file: %w", err)
	}
	timeZone, err := validateTimeZone(settings.TimeZone)
	if err != nil {
		return fmt.Errorf("invalid settings file: %w", err)
	}
	note, err := validateNote(settings.Note)
	if err != nil {
		return fmt.Errorf("invalid settings file: %w", err)
	}
	settings.SiteName = name
	settings.SiteSubtitle = subtitle
	settings.TimeZone = timeZone
	settings.Note = note
	settings.GlassColor = color
	p.data = settings
	return nil
}

func (p *preferencesStore) get() sitePreferences {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.data
}

func (p *preferencesStore) updateSiteName(value string) error {
	return p.update(preferencesInput{SiteName: &value})
}

func (p *preferencesStore) update(input preferencesInput) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	before := p.data
	if input.SiteName != nil {
		name, err := validateSiteName(*input.SiteName)
		if err != nil {
			return err
		}
		p.data.SiteName = name
	}
	if input.SiteSubtitle != nil {
		subtitle, err := validateSiteSubtitle(*input.SiteSubtitle)
		if err != nil {
			return err
		}
		p.data.SiteSubtitle = subtitle
	}
	if input.TimeZone != nil {
		timeZone, err := validateTimeZone(*input.TimeZone)
		if err != nil {
			return err
		}
		p.data.TimeZone = timeZone
	}
	if input.Note != nil {
		note, err := validateNote(*input.Note)
		if err != nil {
			return err
		}
		p.data.Note = note
	}
	if input.GlassEnabled != nil {
		p.data.GlassEnabled = *input.GlassEnabled
	}
	if input.GlassColor != nil {
		color, err := validateGlassColor(*input.GlassColor)
		if err != nil {
			return err
		}
		p.data.GlassColor = color
	}
	if err := p.saveLocked(); err != nil {
		p.data = before
		return err
	}
	return nil
}

func (p *preferencesStore) saveLocked() error {
	raw, err := json.MarshalIndent(p.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if len(raw) > maxSettingsBytes {
		return errors.New("settings exceed safety limit")
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		if removeErr := os.Remove(p.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace settings file: %w", err)
		}
		if retryErr := os.Rename(tmp, p.path); retryErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace settings file: %w", retryErr)
		}
	}
	return nil
}

func validateSiteName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || runeCount(value) > maxSiteNameRunes || strings.ContainsAny(value, "\r\n\t") {
		return "", fmt.Errorf("站点名称必填，且不能超过 %d 个字符", maxSiteNameRunes)
	}
	return value, nil
}

func validateSiteSubtitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if runeCount(value) > maxSiteSubtitleRunes || strings.ContainsAny(value, "\r\n\t") {
		return "", fmt.Errorf("首页副标题不能超过 %d 个字符或包含换行", maxSiteSubtitleRunes)
	}
	return value, nil
}

func validateTimeZone(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "UTC", "Asia/Hong_Kong", "Asia/Shanghai", "Asia/Taipei", "Asia/Tokyo", "Asia/Singapore", "Asia/Bangkok", "Asia/Dubai", "Europe/London", "Europe/Paris", "America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles", "Australia/Sydney":
		return value, nil
	default:
		return "", errors.New("请选择支持的时区")
	}
}

func validateNote(value string) (string, error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	if runeCount(value) > maxNoteRunes {
		return "", fmt.Errorf("便签不能超过 %d 个字符", maxNoteRunes)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("便签包含无效字符")
	}
	return value, nil
}

func validateGlassColor(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 7 || value[0] != '#' {
		return "", errors.New("毛玻璃颜色必须使用十六进制颜色")
	}
	for _, char := range value[1:] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", errors.New("毛玻璃颜色必须使用十六进制颜色")
		}
	}
	return value, nil
}

type credentialData struct {
	Version        int    `json:"version"`
	Iterations     int    `json:"iterations"`
	Salt           string `json:"salt"`
	Hash           string `json:"hash"`
	SessionVersion uint64 `json:"sessionVersion"`
}

type credentialStore struct {
	mu   sync.RWMutex
	path string
	data credentialData
}

var errCurrentPassword = errors.New("当前密码错误")

func newCredentialStore(path, initialPassword string) (*credentialStore, error) {
	credentials := &credentialStore{path: path}
	if err := credentials.loadOrCreate(initialPassword); err != nil {
		return nil, err
	}
	return credentials, nil
}

func (c *credentialStore) loadOrCreate(initialPassword string) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	info, err := os.Stat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := validateInitialPassword(initialPassword); err != nil {
			return err
		}
		data, err := newCredentialData(initialPassword, 1)
		if err != nil {
			return err
		}
		c.data = data
		return c.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("inspect credentials file: %w", err)
	}
	if info.Size() > maxSettingsBytes {
		return fmt.Errorf("credentials file exceeds %d KiB safety limit", maxSettingsBytes>>10)
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("read credentials file: %w", err)
	}
	var data credentialData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode credentials file: %w", err)
	}
	if err := validateCredentialData(data); err != nil {
		return fmt.Errorf("invalid credentials file: %w", err)
	}
	c.data = data
	return nil
}

func newCredentialData(password string, sessionVersion uint64) (credentialData, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return credentialData{}, fmt.Errorf("generate password salt: %w", err)
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordHashBytes)
	if err != nil {
		return credentialData{}, fmt.Errorf("derive password hash: %w", err)
	}
	return credentialData{
		Version:        1,
		Iterations:     passwordIterations,
		Salt:           base64.RawStdEncoding.EncodeToString(salt),
		Hash:           base64.RawStdEncoding.EncodeToString(hash),
		SessionVersion: sessionVersion,
	}, nil
}

func validateCredentialData(data credentialData) error {
	if data.Version != 1 || data.Iterations < 100_000 || data.Iterations > 5_000_000 || data.SessionVersion == 0 {
		return errors.New("凭据参数无效")
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(data.Salt)
	hash, hashErr := base64.RawStdEncoding.DecodeString(data.Hash)
	if saltErr != nil || hashErr != nil || len(salt) < passwordSaltBytes || len(hash) != passwordHashBytes {
		return errors.New("凭据内容无效")
	}
	return nil
}

func validatePassword(password string) error {
	if runeCount(password) < 8 || len(password) > 256 || strings.TrimSpace(password) == "" {
		return errors.New("密码至少需要 8 个字符，且不能超过 256 字节")
	}
	return nil
}

func validateInitialPassword(password string) error {
	if password == defaultInitialPassword {
		return nil
	}
	return validatePassword(password)
}

func (c *credentialStore) verify(password string) bool {
	c.mu.RLock()
	data := c.data
	c.mu.RUnlock()
	return verifyCredentialData(data, password)
}

func verifyCredentialData(data credentialData, password string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(data.Salt)
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(data.Hash)
	if err != nil {
		return false
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, data.Iterations, len(expected))
	return err == nil && subtle.ConstantTimeCompare(derived, expected) == 1
}

func (c *credentialStore) change(currentPassword, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(currentPassword) > 256 || !verifyCredentialData(c.data, currentPassword) {
		return errCurrentPassword
	}
	if currentPassword == newPassword {
		return errors.New("新密码不能与当前密码相同")
	}
	next, err := newCredentialData(newPassword, c.data.SessionVersion+1)
	if err != nil {
		return err
	}
	before := c.data
	c.data = next
	if err := c.saveLocked(); err != nil {
		c.data = before
		return err
	}
	return nil
}

func (c *credentialStore) sessionVersion() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.SessionVersion
}

func (c *credentialStore) saveLocked() error {
	raw, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		if removeErr := os.Remove(c.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace credentials file: %w", err)
		}
		if retryErr := os.Rename(tmp, c.path); retryErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace credentials file: %w", retryErr)
		}
	}
	return nil
}

type store struct {
	mu   sync.RWMutex
	path string
	data database
}

func newStore(path string) (*store, error) {
	s := &store{path: path, data: database{Version: 1, Bookmarks: []bookmark{}}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect data file: %w", err)
	}
	if info.Size() > maxDataBytes {
		return fmt.Errorf("data file exceeds %d MiB safety limit", maxDataBytes>>20)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read data file: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var data database
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode data file: %w", err)
	}
	if len(data.Bookmarks) > maxBookmarks {
		return fmt.Errorf("data file contains more than %d bookmarks", maxBookmarks)
	}
	if data.Version == 0 {
		data.Version = 1
	}
	s.data = data
	return nil
}

func (s *store) list() []bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]bookmark, len(s.data.Bookmarks))
	copy(out, s.data.Bookmarks)
	return out
}

func (s *store) get(id string) (bookmark, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Bookmarks {
		if item.ID == id {
			return item, true
		}
	}
	return bookmark{}, false
}

func (s *store) setIcon(id string, hasIcon bool) (bookmark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Bookmarks {
		if s.data.Bookmarks[i].ID != id {
			continue
		}
		before := s.data.Bookmarks[i]
		s.data.Bookmarks[i].HasIcon = hasIcon
		if hasIcon {
			s.data.Bookmarks[i].IconVersion = time.Now().UnixNano()
		} else {
			s.data.Bookmarks[i].IconVersion = 0
		}
		s.data.Bookmarks[i].UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			s.data.Bookmarks[i] = before
			return bookmark{}, err
		}
		return s.data.Bookmarks[i], nil
	}
	return bookmark{}, os.ErrNotExist
}

func (s *store) create(input bookmarkInput) (bookmark, error) {
	clean, err := validateBookmark(input)
	if err != nil {
		return bookmark{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Bookmarks) >= maxBookmarks {
		return bookmark{}, fmt.Errorf("书签数量已达到 %d 条上限", maxBookmarks)
	}
	now := time.Now().UTC()
	item := bookmark{
		ID:          newID(),
		Title:       clean.Title,
		URL:         clean.URL,
		Category:    clean.Category,
		Description: clean.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.data.Bookmarks = append(s.data.Bookmarks, item)
	if err := s.saveLocked(); err != nil {
		s.data.Bookmarks = s.data.Bookmarks[:len(s.data.Bookmarks)-1]
		return bookmark{}, err
	}
	return item, nil
}

func (s *store) update(id string, input bookmarkInput) (bookmark, error) {
	clean, err := validateBookmark(input)
	if err != nil {
		return bookmark{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Bookmarks {
		if s.data.Bookmarks[i].ID != id {
			continue
		}
		before := s.data.Bookmarks[i]
		s.data.Bookmarks[i].Title = clean.Title
		s.data.Bookmarks[i].URL = clean.URL
		s.data.Bookmarks[i].Category = clean.Category
		s.data.Bookmarks[i].Description = clean.Description
		s.data.Bookmarks[i].UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			s.data.Bookmarks[i] = before
			return bookmark{}, err
		}
		return s.data.Bookmarks[i], nil
	}
	return bookmark{}, os.ErrNotExist
}

func (s *store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Bookmarks {
		if s.data.Bookmarks[i].ID != id {
			continue
		}
		before := append([]bookmark(nil), s.data.Bookmarks...)
		s.data.Bookmarks = append(s.data.Bookmarks[:i], s.data.Bookmarks[i+1:]...)
		if err := s.saveLocked(); err != nil {
			s.data.Bookmarks = before
			return err
		}
		return nil
	}
	return os.ErrNotExist
}

func (s *store) importMany(inputs []bookmarkInput, mode string) (int, error) {
	if len(inputs) == 0 {
		return 0, errors.New("没有可导入的书签")
	}
	if len(inputs) > maxBookmarks {
		return 0, fmt.Errorf("单次最多导入 %d 条书签", maxBookmarks)
	}
	cleaned := make([]bookmarkInput, 0, len(inputs))
	inputSeen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		item, err := validateBookmark(input)
		if err != nil {
			return 0, err
		}
		key := strings.ToLower(item.URL)
		if _, ok := inputSeen[key]; ok {
			continue
		}
		inputSeen[key] = struct{}{}
		cleaned = append(cleaned, item)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != "merge" && mode != "replace" {
		return 0, errors.New("导入模式无效")
	}
	before := append([]bookmark(nil), s.data.Bookmarks...)
	existing := make(map[string]struct{}, len(s.data.Bookmarks)+len(cleaned))
	if mode == "replace" {
		s.data.Bookmarks = s.data.Bookmarks[:0]
	} else {
		for _, item := range s.data.Bookmarks {
			existing[strings.ToLower(item.URL)] = struct{}{}
		}
	}

	added := 0
	now := time.Now().UTC()
	for _, item := range cleaned {
		key := strings.ToLower(item.URL)
		if mode == "merge" {
			if _, exists := existing[key]; exists {
				continue
			}
		}
		if len(s.data.Bookmarks) >= maxBookmarks {
			s.data.Bookmarks = before
			return 0, fmt.Errorf("导入后将超过 %d 条书签上限", maxBookmarks)
		}
		s.data.Bookmarks = append(s.data.Bookmarks, bookmark{
			ID:          newID(),
			Title:       item.Title,
			URL:         item.URL,
			Category:    item.Category,
			Description: item.Description,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		existing[key] = struct{}{}
		added++
	}
	if err := s.saveLocked(); err != nil {
		s.data.Bookmarks = before
		return 0, err
	}
	return added, nil
}

func (s *store) saveLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode data: %w", err)
	}
	if len(raw) > maxDataBytes {
		return fmt.Errorf("data exceeds %d MiB safety limit", maxDataBytes>>20)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace data file: %w", err)
		}
		if retryErr := os.Rename(tmp, s.path); retryErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace data file: %w", retryErr)
		}
	}
	return nil
}

func validateBookmark(input bookmarkInput) (bookmarkInput, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.URL = strings.TrimSpace(input.URL)
	input.Category = strings.TrimSpace(input.Category)
	input.Description = strings.TrimSpace(input.Description)
	if input.Category == "" {
		input.Category = "未分类"
	}
	if input.Title == "" || runeCount(input.Title) > 120 {
		return bookmarkInput{}, errors.New("标题必填，且不能超过 120 个字符")
	}
	if input.URL == "" || len(input.URL) > 2048 {
		return bookmarkInput{}, errors.New("网址必填，且不能超过 2048 个字符")
	}
	parsed, err := url.Parse(input.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return bookmarkInput{}, errors.New("网址必须以 http:// 或 https:// 开头")
	}
	if runeCount(input.Category) > 40 {
		return bookmarkInput{}, errors.New("分类不能超过 40 个字符")
	}
	if runeCount(input.Description) > 300 {
		return bookmarkInput{}, errors.New("备注不能超过 300 个字符")
	}
	return input, nil
}

func runeCount(value string) int {
	return utf8.RuneCountInString(value)
}

func newID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		panic("secure random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

type siteHealth struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	HTTPCode  int       `json:"httpCode,omitempty"`
	LatencyMS int64     `json:"latencyMs,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	Error     string    `json:"error,omitempty"`
}

type healthSnapshot struct {
	Running      bool                  `json:"running"`
	Total        int                   `json:"total"`
	Completed    int                   `json:"completed"`
	StartedAt    time.Time             `json:"startedAt,omitempty"`
	AllowPrivate bool                  `json:"allowPrivate"`
	Results      map[string]siteHealth `json:"results"`
}

type healthChecker struct {
	allowPrivate bool
	resolver     *net.Resolver
	dialer       *net.Dialer
	client       *http.Client
}

func newHealthChecker(allowPrivate bool) *healthChecker {
	checker := &healthChecker{
		allowPrivate: allowPrivate,
		resolver:     net.DefaultResolver,
		dialer:       &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 20 * time.Second},
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            checker.dialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           12,
		MaxIdleConnsPerHost:    checkWorkers,
		MaxConnsPerHost:        checkWorkers,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    4 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
		DisableCompression:     true,
	}
	checker.client = &http.Client{
		Transport: transport,
		Timeout:   checkTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect scheme is not allowed")
			}
			return nil
		},
	}
	return checker
}

func (c *healthChecker) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid target address: %w", err)
	}
	addresses, err := c.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	allowed := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if c.allowPrivate || !isBlockedTargetIP(address.IP) {
			allowed = append(allowed, address.IP)
		}
	}
	if len(allowed) == 0 {
		return nil, errors.New("target resolves to a private or reserved address")
	}
	var lastErr error
	for _, ip := range allowed {
		connection, dialErr := c.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

func isBlockedTargetIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4[0] == 0 || (ipv4[0] == 100 && ipv4[1]&0xc0 == 64) || ipv4[0] >= 224
	}
	return false
}

func (c *healthChecker) check(item bookmark) siteHealth {
	started := time.Now()
	result := siteHealth{ID: item.ID, Status: "down", CheckedAt: started.UTC()}
	request, err := http.NewRequest(http.MethodGet, item.URL, nil)
	if err != nil {
		result.Error = "网址格式无效"
		return result
	}
	request.Header.Set("User-Agent", "Lightmarks-Health/1.0")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.8,*/*;q=0.5")
	request.Header.Set("Range", "bytes=0-0")
	response, err := c.client.Do(request)
	result.LatencyMS = time.Since(started).Milliseconds()
	result.CheckedAt = time.Now().UTC()
	if err != nil {
		result.Error = friendlyCheckError(err)
		return result
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxCheckDrainBytes))
	_ = response.Body.Close()
	result.HTTPCode = response.StatusCode
	if response.StatusCode < http.StatusInternalServerError {
		result.Status = "online"
		return result
	}
	result.Error = fmt.Sprintf("服务器返回状态码 %d", response.StatusCode)
	return result
}

func friendlyCheckError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "连接超时"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "private or reserved") {
		return "私有或保留地址已被保护"
	}
	if strings.Contains(message, "no such host") || strings.Contains(message, "resolve target") {
		return "域名解析失败"
	}
	if strings.Contains(message, "certificate") || strings.Contains(message, "tls") {
		return "网站证书异常"
	}
	return "无法建立连接"
}

type healthManager struct {
	mu           sync.RWMutex
	checker      *healthChecker
	results      map[string]siteHealth
	running      bool
	total        int
	completed    int
	startedAt    time.Time
	allowPrivate bool
	onComplete   func([]bookmark, map[string]siteHealth, map[string]siteHealth)
}

func newHealthManager(allowPrivate bool) *healthManager {
	return &healthManager{
		checker:      newHealthChecker(allowPrivate),
		results:      make(map[string]siteHealth),
		allowPrivate: allowPrivate,
	}
}

func (m *healthManager) snapshot() healthSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	results := make(map[string]siteHealth, len(m.results))
	for id, result := range m.results {
		results[id] = result
	}
	return healthSnapshot{
		Running:      m.running,
		Total:        m.total,
		Completed:    m.completed,
		StartedAt:    m.startedAt,
		AllowPrivate: m.allowPrivate,
		Results:      results,
	}
}

func (m *healthManager) start(items []bookmark) bool {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return false
	}
	validIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		validIDs[item.ID] = struct{}{}
	}
	for id := range m.results {
		if _, exists := validIDs[id]; !exists {
			delete(m.results, id)
		}
	}
	previous := cloneHealthResults(m.results)
	m.running = len(items) > 0
	m.total = len(items)
	m.completed = 0
	m.startedAt = time.Now().UTC()
	m.mu.Unlock()
	if len(items) == 0 {
		return true
	}
	go m.run(items, previous)
	return true
}

func (m *healthManager) run(items []bookmark, previous map[string]siteHealth) {
	jobs := make(chan bookmark)
	var workers sync.WaitGroup
	workerCount := min(checkWorkers, len(items))
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for item := range jobs {
				result := m.checker.check(item)
				m.mu.Lock()
				m.results[item.ID] = result
				m.completed++
				m.mu.Unlock()
			}
		}()
	}
	for _, item := range items {
		jobs <- item
	}
	close(jobs)
	workers.Wait()
	m.mu.Lock()
	m.running = false
	current := cloneHealthResults(m.results)
	onComplete := m.onComplete
	m.mu.Unlock()
	if onComplete != nil {
		onComplete(items, previous, current)
	}
}

func (m *healthManager) setOnComplete(callback func([]bookmark, map[string]siteHealth, map[string]siteHealth)) {
	m.mu.Lock()
	m.onComplete = callback
	m.mu.Unlock()
}

func cloneHealthResults(results map[string]siteHealth) map[string]siteHealth {
	cloned := make(map[string]siteHealth, len(results))
	for id, result := range results {
		cloned[id] = result
	}
	return cloned
}

func (m *healthManager) forget(id string) {
	m.mu.Lock()
	delete(m.results, id)
	m.mu.Unlock()
}

type app struct {
	store          *store
	preferences    *preferencesStore
	credentials    *credentialStore
	health         *healthManager
	notifications  *notificationStore
	notifier       *notificationService
	scheduler      *monitorScheduler
	backgroundPath string
	iconsDir       string
	iconMu         sync.Mutex
	secret         []byte
	loginMu        sync.Mutex
	loginFails     int
	blockedUntil   time.Time
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/session", a.handleSession)
	mux.HandleFunc("POST /api/password", a.requireAuth(a.handlePasswordChange))
	mux.HandleFunc("GET /api/bookmarks", a.requireAuth(a.handleList))
	mux.HandleFunc("POST /api/bookmarks", a.requireAuth(a.handleCreate))
	mux.HandleFunc("PUT /api/bookmarks/{id}", a.requireAuth(a.handleUpdate))
	mux.HandleFunc("DELETE /api/bookmarks/{id}", a.requireAuth(a.handleDelete))
	mux.HandleFunc("GET /api/bookmarks/{id}/icon", a.requireAuth(a.handleIcon))
	mux.HandleFunc("POST /api/bookmarks/{id}/icon", a.requireAuth(a.handleIconUpload))
	mux.HandleFunc("DELETE /api/bookmarks/{id}/icon", a.requireAuth(a.handleIconDelete))
	mux.HandleFunc("POST /api/bookmarks/{id}/icon/detect", a.requireAuth(a.handleIconDetect))
	mux.HandleFunc("POST /api/import", a.requireAuth(a.handleImport))
	mux.HandleFunc("GET /api/export", a.requireAuth(a.handleExport))
	mux.HandleFunc("GET /api/health", a.requireAuth(a.handleHealth))
	mux.HandleFunc("POST /api/health/check", a.requireAuth(a.handleHealthCheck))
	mux.HandleFunc("GET /api/settings", a.handleSettings)
	mux.HandleFunc("PUT /api/settings", a.requireAuth(a.handleSettingsUpdate))
	mux.HandleFunc("GET /api/notifications", a.requireAuth(a.handleNotificationSettings))
	mux.HandleFunc("PUT /api/notifications", a.requireAuth(a.handleNotificationSettingsUpdate))
	mux.HandleFunc("POST /api/notifications/test", a.requireAuth(a.handleNotificationTest))
	mux.HandleFunc("GET /api/note", a.requireAuth(a.handleNote))
	mux.HandleFunc("PUT /api/note", a.requireAuth(a.handleNoteUpdate))
	mux.HandleFunc("GET /api/background", a.requireAuth(a.handleBackground))
	mux.HandleFunc("POST /api/background", a.requireAuth(a.handleBackgroundUpload))
	mux.HandleFunc("DELETE /api/background", a.requireAuth(a.handleBackgroundDelete))

	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return securityHeaders(mux)
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	a.loginMu.Lock()
	if time.Now().Before(a.blockedUntil) {
		retry := time.Until(a.blockedUntil).Round(time.Second)
		a.loginMu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	a.loginMu.Unlock()

	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !a.credentials.verify(body.Password) {
		a.loginMu.Lock()
		a.loginFails++
		if a.loginFails >= 5 {
			a.blockedUntil = time.Now().Add(30 * time.Second)
			a.loginFails = 0
		}
		a.loginMu.Unlock()
		writeError(w, http.StatusUnauthorized, "密码错误")
		return
	}
	a.loginMu.Lock()
	a.loginFails = 0
	a.blockedUntil = time.Time{}
	a.loginMu.Unlock()
	a.setSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	a.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.credentials.change(body.CurrentPassword, body.NewPassword); err != nil {
		if errors.Is(err, errCurrentPassword) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "lightmarks_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

func (a *app) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": a.authenticated(r)})
}

func (a *app) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"bookmarks": a.store.list(),
		"limit":     maxBookmarks,
	})
}

func (a *app) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input bookmarkInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := a.store.create(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input bookmarkInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := a.store.update(r.PathValue("id"), input)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "书签不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.health.forget(item.ID)
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	id := r.PathValue("id")
	if err := a.store.remove(id); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "书签不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	a.health.forget(id)
	a.iconMu.Lock()
	_ = os.Remove(a.iconPath(id))
	a.iconMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) iconPath(id string) string {
	return filepath.Join(a.iconsDir, id+".image")
}

func (a *app) handleIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	item, exists := a.store.get(id)
	if !exists || !item.HasIcon {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(a.iconPath(id))
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "网站图片读取失败")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "网站图片读取失败")
		return
	}
	header := make([]byte, 512)
	read, _ := file.Read(header)
	_, _ = file.Seek(0, io.SeekStart)
	contentType := http.DetectContentType(header[:read])
	if isICO(header[:read]) {
		contentType = "image/x-icon"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "site-icon", info.ModTime(), file)
}

func (a *app) handleIconUpload(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	id := r.PathValue("id")
	if _, exists := a.store.get(id); !exists {
		writeError(w, http.StatusNotFound, "书签不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxIconBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "网站图片无效或超过 256 KiB")
		return
	}
	if _, err := validateSiteIcon(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := a.saveIcon(id, raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "网站图片保存失败")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleIconDetect(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	id := r.PathValue("id")
	item, exists := a.store.get(id)
	if !exists {
		writeError(w, http.StatusNotFound, "书签不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	raw, err := a.detectBookmarkIcon(ctx, item)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	updated, err := a.saveIcon(id, raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "网站图片保存失败")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *app) handleIconDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	id := r.PathValue("id")
	if _, exists := a.store.get(id); !exists {
		writeError(w, http.StatusNotFound, "书签不存在")
		return
	}
	a.iconMu.Lock()
	err := os.Remove(a.iconPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.iconMu.Unlock()
		writeError(w, http.StatusInternalServerError, "网站图片删除失败")
		return
	}
	item, err := a.store.setIcon(id, false)
	a.iconMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "网站图片状态保存失败")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) saveIcon(id string, raw []byte) (bookmark, error) {
	a.iconMu.Lock()
	defer a.iconMu.Unlock()
	path := a.iconPath(id)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return bookmark{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(temporary)
			return bookmark{}, err
		}
		if retryErr := os.Rename(temporary, path); retryErr != nil {
			_ = os.Remove(temporary)
			return bookmark{}, retryErr
		}
	}
	item, err := a.store.setIcon(id, true)
	if err != nil {
		_ = os.Remove(path)
		return bookmark{}, err
	}
	return item, nil
}

func (a *app) pruneIcons() {
	valid := make(map[string]struct{})
	for _, item := range a.store.list() {
		valid[item.ID] = struct{}{}
	}
	entries, err := os.ReadDir(a.iconsDir)
	if err != nil {
		return
	}
	a.iconMu.Lock()
	defer a.iconMu.Unlock()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".image") {
			continue
		}
		id := strings.TrimSuffix(name, ".image")
		if _, exists := valid[id]; !exists {
			_ = os.Remove(filepath.Join(a.iconsDir, name))
		}
	}
}

func (a *app) detectBookmarkIcon(ctx context.Context, item bookmark) ([]byte, error) {
	parsed, err := url.Parse(item.URL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("网址格式无效")
	}
	origin := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	paths := []string{"/favicon.ico", "/favicon.png", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png"}
	for _, candidatePath := range paths {
		candidate := *origin
		candidate.Path = candidatePath
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.String(), nil)
		if err != nil {
			continue
		}
		request.Header.Set("User-Agent", "Lightmarks-Icon/1.0")
		request.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/x-icon,*/*;q=0.2")
		response, err := a.health.checker.client.Do(request)
		if err != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxIconBytes+1))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(raw) > maxIconBytes {
			continue
		}
		if _, err := validateSiteIcon(raw); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("未检测到可用网站图片，请手动导入")
}

func validateSiteIcon(raw []byte) (string, error) {
	if len(raw) == 0 || len(raw) > maxIconBytes {
		return "", errors.New("网站图片无效或超过 256 KiB")
	}
	if isICO(raw) {
		count := int(raw[4]) | int(raw[5])<<8
		if count == 0 || count > 64 || len(raw) < 6+count*16 {
			return "", errors.New("ICO 图片内容无效")
		}
		for i := 0; i < count; i++ {
			entryOffset := 6 + i*16
			width, height := int(raw[entryOffset]), int(raw[entryOffset+1])
			if width == 0 {
				width = 256
			}
			if height == 0 {
				height = 256
			}
			if width*height > maxIconPixels {
				return "", errors.New("网站图片像素过大")
			}
			imageBytes := binary.LittleEndian.Uint32(raw[entryOffset+8 : entryOffset+12])
			imageOffset := binary.LittleEndian.Uint32(raw[entryOffset+12 : entryOffset+16])
			if imageBytes == 0 || imageOffset < uint32(6+count*16) || uint64(imageOffset)+uint64(imageBytes) > uint64(len(raw)) {
				return "", errors.New("ICO 图片内容无效")
			}
		}
		return "image/x-icon", nil
	}
	contentType := http.DetectContentType(raw)
	if contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/gif" {
		return "", errors.New("网站图片仅支持 JPG、PNG、GIF 或 ICO")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return "", errors.New("网站图片内容无效")
	}
	if int64(config.Width)*int64(config.Height) > maxIconPixels {
		return "", errors.New("网站图片像素过大，请控制在 400 万像素以内")
	}
	return contentType, nil
}

func isICO(raw []byte) bool {
	return len(raw) >= 6 && raw[0] == 0 && raw[1] == 0 && raw[2] == 1 && raw[3] == 0
}

func (a *app) handleImport(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var body struct {
		Bookmarks []bookmarkInput `json:"bookmarks"`
		Mode      string          `json:"mode"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	added, err := a.store.importMany(body.Bookmarks, body.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.pruneIcons()
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

func (a *app) handleExport(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Disposition", "attachment; filename=lightmarks-backup.json")
	writeJSON(w, http.StatusOK, database{Version: 1, Bookmarks: a.store.list()})
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.health.snapshot())
}

func (a *app) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	started := a.health.start(a.store.list())
	snapshot := a.health.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":   started,
		"running":   snapshot.Running,
		"total":     snapshot.Total,
		"completed": snapshot.Completed,
	})
}

type settingsResponse struct {
	SiteName           string    `json:"siteName"`
	SiteSubtitle       string    `json:"siteSubtitle"`
	TimeZone           string    `json:"timeZone"`
	GlassEnabled       bool      `json:"glassEnabled"`
	GlassColor         string    `json:"glassColor"`
	HasBackground      bool      `json:"hasBackground"`
	BackgroundUpdated  time.Time `json:"backgroundUpdatedAt,omitempty"`
	MaxBackgroundBytes int       `json:"maxBackgroundBytes"`
}

func (a *app) currentSettings() settingsResponse {
	preferences := a.preferences.get()
	settings := settingsResponse{
		SiteName:           preferences.SiteName,
		SiteSubtitle:       preferences.SiteSubtitle,
		TimeZone:           preferences.TimeZone,
		GlassEnabled:       preferences.GlassEnabled,
		GlassColor:         preferences.GlassColor,
		MaxBackgroundBytes: maxBackgroundBytes,
	}
	info, err := os.Stat(a.backgroundPath)
	if err == nil && info.Mode().IsRegular() {
		settings.HasBackground = true
		settings.BackgroundUpdated = info.ModTime().UTC()
	}
	return settings
}

func (a *app) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.currentSettings())
}

func (a *app) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input preferencesInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.SiteName == nil && input.SiteSubtitle == nil && input.TimeZone == nil && input.GlassEnabled == nil && input.GlassColor == nil {
		writeError(w, http.StatusBadRequest, "没有可保存的设置")
		return
	}
	if err := a.preferences.update(input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.currentSettings())
}

type noteInput struct {
	Note string `json:"note"`
}

func (a *app) handleNote(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, noteInput{Note: a.preferences.get().Note})
}

func (a *app) handleNoteUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input noteInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.preferences.update(preferencesInput{Note: &input.Note}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, noteInput{Note: a.preferences.get().Note})
}

func (a *app) handleBackground(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(a.backgroundPath)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "背景图片读取失败")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "背景图片读取失败")
		return
	}
	header := make([]byte, 512)
	read, _ := file.Read(header)
	_, _ = file.Seek(0, io.SeekStart)
	w.Header().Set("Content-Type", http.DetectContentType(header[:read]))
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, "background", info.ModTime(), file)
}

func (a *app) handleBackgroundUpload(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBackgroundBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "背景图片无效或超过 2 MiB")
		return
	}
	if err := validateBackground(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	temporary := a.backgroundPath + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "背景图片保存失败")
		return
	}
	if err := os.Rename(temporary, a.backgroundPath); err != nil {
		if removeErr := os.Remove(a.backgroundPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(temporary)
			writeError(w, http.StatusInternalServerError, "背景图片保存失败")
			return
		}
		if retryErr := os.Rename(temporary, a.backgroundPath); retryErr != nil {
			_ = os.Remove(temporary)
			writeError(w, http.StatusInternalServerError, "背景图片保存失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, a.currentSettings())
}

func validateBackground(raw []byte) error {
	contentType := http.DetectContentType(raw)
	if contentType != "image/jpeg" && contentType != "image/png" {
		return errors.New("背景图片仅支持 JPG 或 PNG 格式")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return errors.New("背景图片内容无效")
	}
	if int64(config.Width)*int64(config.Height) > maxBackgroundPixels {
		return errors.New("背景图片像素过大，请控制在 2000 万像素以内")
	}
	return nil
}

func (a *app) handleBackgroundDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	if err := os.Remove(a.backgroundPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "背景图片删除失败")
		return
	}
	writeJSON(w, http.StatusOK, a.currentSettings())
}

func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "登录已过期")
			return
		}
		next(w, r)
	}
}

func (a *app) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(sessionDuration)
	payload := strconv.FormatInt(expires.Unix(), 10) + ":" + strconv.FormatUint(a.credentials.sessionVersion(), 10)
	signature := sign(a.secret, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     "lightmarks_session",
		Value:    payload + "." + signature,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

func (a *app) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie("lightmarks_session")
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(sign(a.secret, parts[0]))) {
		return false
	}
	payloadParts := strings.Split(parts[0], ":")
	if len(payloadParts) < 1 || len(payloadParts) > 2 {
		return false
	}
	expires, err := strconv.ParseInt(payloadParts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	sessionVersion := uint64(1)
	if len(payloadParts) == 2 {
		sessionVersion, err = strconv.ParseUint(payloadParts[1], 10, 64)
		if err != nil {
			return false
		}
	}
	return sessionVersion == a.credentials.sessionVersion()
}

func sign(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("请求内容无效或超过 1 MiB")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func randomSecret() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("secure random source unavailable")
	}
	return secret
}

func main() {
	memoryLimit := int64(48 << 20)
	if value, err := strconv.Atoi(os.Getenv("MEMORY_LIMIT_MB")); err == nil && value >= 32 && value <= 512 {
		memoryLimit = int64(value) << 20
	}
	debug.SetMemoryLimit(memoryLimit)

	password := os.Getenv("BOOKMARK_PASSWORD")
	if err := validateInitialPassword(password); err != nil {
		log.Fatal("BOOKMARK_PASSWORD must contain at least 8 characters, or use the default initial password")
	}
	secret := []byte(os.Getenv("SESSION_SECRET"))
	if len(secret) < 32 {
		secret = randomSecret()
		log.Print("SESSION_SECRET is not set or too short; sessions will reset after restart")
	}
	dataPath := os.Getenv("DATA_FILE")
	if dataPath == "" {
		dataPath = filepath.Join("data", "bookmarks.json")
	}
	dataStore, err := newStore(dataPath)
	if err != nil {
		log.Fatal(err)
	}
	preferences, err := newPreferencesStore(filepath.Join(filepath.Dir(dataPath), "settings.json"))
	if err != nil {
		log.Fatal(err)
	}
	credentials, err := newCredentialStore(filepath.Join(filepath.Dir(dataPath), "credentials.json"), password)
	if err != nil {
		log.Fatal(err)
	}
	iconsDir := filepath.Join(filepath.Dir(dataPath), "icons")
	if err := os.MkdirAll(iconsDir, 0o700); err != nil {
		log.Fatal(err)
	}
	notifications, err := newNotificationStore(filepath.Join(filepath.Dir(dataPath), "notifications.json"), secret)
	if err != nil {
		log.Fatal(err)
	}
	allowPrivateTargets := strings.EqualFold(os.Getenv("ALLOW_PRIVATE_TARGETS"), "true")
	health := newHealthManager(allowPrivateTargets)
	notifier := newNotificationService(notifications)
	application := &app{
		store:          dataStore,
		preferences:    preferences,
		credentials:    credentials,
		health:         health,
		notifications:  notifications,
		notifier:       notifier,
		backgroundPath: filepath.Join(filepath.Dir(dataPath), "background.image"),
		iconsDir:       iconsDir,
		secret:         secret,
	}
	application.scheduler = newMonitorScheduler(dataStore, health, notifications)
	health.setOnComplete(func(items []bookmark, previous, current map[string]siteHealth) {
		settings := preferences.get()
		notifier.notifyChanges(settings.SiteName, settings.TimeZone, collectHealthChanges(items, previous, current))
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "5856"
	}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           application.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go application.scheduler.run(ctx)
	go func() {
		log.Printf("Lightmarks listening on :%s", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
