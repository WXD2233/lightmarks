package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

const (
	defaultMonitorIntervalMinutes = 10
	minMonitorIntervalMinutes     = 1
	maxMonitorIntervalMinutes     = 24 * 60
	maxNotificationChanges        = 30
	notificationSendTimeout       = 15 * time.Second
	maxNotificationResponseBytes  = 8 << 10
	notificationAAD               = "lightmarks-notifications-v1"
)

type notificationConfig struct {
	MonitorEnabled  bool
	IntervalMinutes int

	TelegramEnabled  bool
	TelegramBotToken string
	TelegramChatID   string

	EmailEnabled bool
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPTo       string
	SMTPSecurity string
}

type notificationDisk struct {
	Version         int  `json:"version"`
	MonitorEnabled  bool `json:"monitorEnabled"`
	IntervalMinutes int  `json:"intervalMinutes"`

	TelegramEnabled  bool   `json:"telegramEnabled"`
	TelegramBotToken string `json:"telegramBotTokenEncrypted,omitempty"`
	TelegramChatID   string `json:"telegramChatId,omitempty"`

	EmailEnabled bool   `json:"emailEnabled"`
	SMTPHost     string `json:"smtpHost,omitempty"`
	SMTPPort     int    `json:"smtpPort,omitempty"`
	SMTPUsername string `json:"smtpUsername,omitempty"`
	SMTPPassword string `json:"smtpPasswordEncrypted,omitempty"`
	SMTPFrom     string `json:"smtpFrom,omitempty"`
	SMTPTo       string `json:"smtpTo,omitempty"`
	SMTPSecurity string `json:"smtpSecurity,omitempty"`
}

type notificationInput struct {
	MonitorEnabled  *bool `json:"monitorEnabled"`
	IntervalMinutes *int  `json:"intervalMinutes"`

	TelegramEnabled       *bool   `json:"telegramEnabled"`
	TelegramBotToken      *string `json:"telegramBotToken"`
	TelegramChatID        *string `json:"telegramChatId"`
	ClearTelegramBotToken bool    `json:"clearTelegramBotToken"`

	EmailEnabled      *bool   `json:"emailEnabled"`
	SMTPHost          *string `json:"smtpHost"`
	SMTPPort          *int    `json:"smtpPort"`
	SMTPUsername      *string `json:"smtpUsername"`
	SMTPPassword      *string `json:"smtpPassword"`
	SMTPFrom          *string `json:"smtpFrom"`
	SMTPTo            *string `json:"smtpTo"`
	SMTPSecurity      *string `json:"smtpSecurity"`
	ClearSMTPPassword bool    `json:"clearSmtpPassword"`
}

type notificationSettingsResponse struct {
	MonitorEnabled  bool `json:"monitorEnabled"`
	IntervalMinutes int  `json:"intervalMinutes"`

	TelegramEnabled     bool   `json:"telegramEnabled"`
	TelegramBotTokenSet bool   `json:"telegramBotTokenSet"`
	TelegramChatID      string `json:"telegramChatId"`

	EmailEnabled    bool   `json:"emailEnabled"`
	SMTPHost        string `json:"smtpHost"`
	SMTPPort        int    `json:"smtpPort"`
	SMTPUsername    string `json:"smtpUsername"`
	SMTPPasswordSet bool   `json:"smtpPasswordSet"`
	SMTPFrom        string `json:"smtpFrom"`
	SMTPTo          string `json:"smtpTo"`
	SMTPSecurity    string `json:"smtpSecurity"`
}

type notificationStore struct {
	mu   sync.RWMutex
	path string
	key  [32]byte
	data notificationConfig
}

func defaultNotificationConfig() notificationConfig {
	return notificationConfig{
		MonitorEnabled:  true,
		IntervalMinutes: defaultMonitorIntervalMinutes,
		SMTPPort:        465,
		SMTPSecurity:    "auto",
	}
}

func newNotificationStore(path string, keyMaterial []byte) (*notificationStore, error) {
	store := &notificationStore{
		path: path,
		key:  sha256.Sum256(keyMaterial),
		data: defaultNotificationConfig(),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *notificationStore) load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create notification settings directory: %w", err)
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect notification settings: %w", err)
	}
	if info.Size() > maxSettingsBytes*2 {
		return errors.New("notification settings exceed safety limit")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read notification settings: %w", err)
	}
	disk := notificationDisk{Version: 1, MonitorEnabled: true, IntervalMinutes: defaultMonitorIntervalMinutes, SMTPPort: 465, SMTPSecurity: "auto"}
	if err := json.Unmarshal(raw, &disk); err != nil {
		return fmt.Errorf("decode notification settings: %w", err)
	}
	if disk.Version != 1 {
		return errors.New("unsupported notification settings version")
	}
	telegramToken, err := decryptNotificationSecret(s.key, disk.TelegramBotToken)
	if err != nil {
		return fmt.Errorf("decrypt Telegram token (check SESSION_SECRET): %w", err)
	}
	smtpPassword, err := decryptNotificationSecret(s.key, disk.SMTPPassword)
	if err != nil {
		return fmt.Errorf("decrypt SMTP password (check SESSION_SECRET): %w", err)
	}
	config := notificationConfig{
		MonitorEnabled:   disk.MonitorEnabled,
		IntervalMinutes:  disk.IntervalMinutes,
		TelegramEnabled:  disk.TelegramEnabled,
		TelegramBotToken: telegramToken,
		TelegramChatID:   disk.TelegramChatID,
		EmailEnabled:     disk.EmailEnabled,
		SMTPHost:         disk.SMTPHost,
		SMTPPort:         disk.SMTPPort,
		SMTPUsername:     disk.SMTPUsername,
		SMTPPassword:     smtpPassword,
		SMTPFrom:         disk.SMTPFrom,
		SMTPTo:           disk.SMTPTo,
		SMTPSecurity:     disk.SMTPSecurity,
	}
	if err := validateNotificationConfig(config); err != nil {
		return fmt.Errorf("invalid notification settings: %w", err)
	}
	s.data = config
	return nil
}

func (s *notificationStore) get() notificationConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *notificationStore) response() notificationSettingsResponse {
	config := s.get()
	return notificationSettingsResponse{
		MonitorEnabled:      config.MonitorEnabled,
		IntervalMinutes:     config.IntervalMinutes,
		TelegramEnabled:     config.TelegramEnabled,
		TelegramBotTokenSet: config.TelegramBotToken != "",
		TelegramChatID:      config.TelegramChatID,
		EmailEnabled:        config.EmailEnabled,
		SMTPHost:            config.SMTPHost,
		SMTPPort:            config.SMTPPort,
		SMTPUsername:        config.SMTPUsername,
		SMTPPasswordSet:     config.SMTPPassword != "",
		SMTPFrom:            config.SMTPFrom,
		SMTPTo:              config.SMTPTo,
		SMTPSecurity:        config.SMTPSecurity,
	}
}

func (s *notificationStore) update(input notificationInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.data
	if input.MonitorEnabled != nil {
		s.data.MonitorEnabled = *input.MonitorEnabled
	}
	if input.IntervalMinutes != nil {
		s.data.IntervalMinutes = *input.IntervalMinutes
	}
	if input.TelegramEnabled != nil {
		s.data.TelegramEnabled = *input.TelegramEnabled
	}
	if input.TelegramChatID != nil {
		s.data.TelegramChatID = strings.TrimSpace(*input.TelegramChatID)
	}
	if input.ClearTelegramBotToken {
		s.data.TelegramBotToken = ""
	} else if input.TelegramBotToken != nil && strings.TrimSpace(*input.TelegramBotToken) != "" {
		s.data.TelegramBotToken = strings.TrimSpace(*input.TelegramBotToken)
	}
	if input.EmailEnabled != nil {
		s.data.EmailEnabled = *input.EmailEnabled
	}
	if input.SMTPHost != nil {
		s.data.SMTPHost = strings.TrimSpace(*input.SMTPHost)
	}
	if input.SMTPPort != nil {
		s.data.SMTPPort = *input.SMTPPort
	}
	if input.SMTPUsername != nil {
		s.data.SMTPUsername = strings.TrimSpace(*input.SMTPUsername)
	}
	if input.ClearSMTPPassword {
		s.data.SMTPPassword = ""
	} else if input.SMTPPassword != nil && *input.SMTPPassword != "" {
		s.data.SMTPPassword = *input.SMTPPassword
	}
	if input.SMTPFrom != nil {
		s.data.SMTPFrom = strings.TrimSpace(*input.SMTPFrom)
	}
	if input.SMTPTo != nil {
		s.data.SMTPTo = strings.TrimSpace(*input.SMTPTo)
	}
	if input.SMTPSecurity != nil {
		s.data.SMTPSecurity = strings.ToLower(strings.TrimSpace(*input.SMTPSecurity))
	}
	if err := validateNotificationConfig(s.data); err != nil {
		s.data = before
		return err
	}
	if err := s.saveLocked(); err != nil {
		s.data = before
		return err
	}
	return nil
}

func (s *notificationStore) saveLocked() error {
	telegramToken, err := encryptNotificationSecret(s.key, s.data.TelegramBotToken)
	if err != nil {
		return err
	}
	smtpPassword, err := encryptNotificationSecret(s.key, s.data.SMTPPassword)
	if err != nil {
		return err
	}
	disk := notificationDisk{
		Version:          1,
		MonitorEnabled:   s.data.MonitorEnabled,
		IntervalMinutes:  s.data.IntervalMinutes,
		TelegramEnabled:  s.data.TelegramEnabled,
		TelegramBotToken: telegramToken,
		TelegramChatID:   s.data.TelegramChatID,
		EmailEnabled:     s.data.EmailEnabled,
		SMTPHost:         s.data.SMTPHost,
		SMTPPort:         s.data.SMTPPort,
		SMTPUsername:     s.data.SMTPUsername,
		SMTPPassword:     smtpPassword,
		SMTPFrom:         s.data.SMTPFrom,
		SMTPTo:           s.data.SMTPTo,
		SMTPSecurity:     s.data.SMTPSecurity,
	}
	raw, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("encode notification settings: %w", err)
	}
	if len(raw) > maxSettingsBytes*2 {
		return errors.New("notification settings exceed safety limit")
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write notification settings: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace notification settings: %w", err)
		}
		if retryErr := os.Rename(tmp, s.path); retryErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace notification settings: %w", retryErr)
		}
	}
	return nil
}

func validateNotificationConfig(config notificationConfig) error {
	if config.IntervalMinutes < minMonitorIntervalMinutes || config.IntervalMinutes > maxMonitorIntervalMinutes {
		return fmt.Errorf("自动检测间隔需要在 %d 到 %d 分钟之间", minMonitorIntervalMinutes, maxMonitorIntervalMinutes)
	}
	if config.TelegramBotToken != "" {
		if len(config.TelegramBotToken) > 256 || !strings.Contains(config.TelegramBotToken, ":") || strings.ContainsAny(config.TelegramBotToken, " \r\n\t") {
			return errors.New("Telegram Bot Token 格式无效")
		}
	}
	if len(config.TelegramChatID) > 128 || strings.ContainsAny(config.TelegramChatID, "\r\n\t") {
		return errors.New("Telegram Chat ID 格式无效")
	}
	if config.TelegramEnabled && (config.TelegramBotToken == "" || config.TelegramChatID == "") {
		return errors.New("启用 Telegram 前需要填写 Bot Token 和 Chat ID")
	}
	if config.SMTPPort < 1 || config.SMTPPort > 65535 {
		return errors.New("SMTP 端口无效")
	}
	if len(config.SMTPHost) > 253 || strings.ContainsAny(config.SMTPHost, " /\r\n\t") {
		return errors.New("SMTP 服务器地址无效")
	}
	if len(config.SMTPUsername) > 320 || strings.ContainsAny(config.SMTPUsername, "\r\n") || len(config.SMTPPassword) > 512 {
		return errors.New("SMTP 账户信息无效")
	}
	if config.SMTPSecurity != "auto" && config.SMTPSecurity != "tls" && config.SMTPSecurity != "starttls" {
		return errors.New("请选择支持的 SMTP 加密方式")
	}
	if config.SMTPFrom != "" {
		if _, err := mail.ParseAddress(config.SMTPFrom); err != nil {
			return errors.New("发件人邮箱格式无效")
		}
	}
	if config.SMTPTo != "" {
		if _, err := mail.ParseAddressList(config.SMTPTo); err != nil {
			return errors.New("收件人邮箱格式无效")
		}
	}
	if config.EmailEnabled {
		if config.SMTPHost == "" || config.SMTPFrom == "" || config.SMTPTo == "" {
			return errors.New("启用邮件前需要填写 SMTP 服务器、发件人和收件人")
		}
		if config.SMTPUsername != "" && config.SMTPPassword == "" {
			return errors.New("SMTP 账户已填写，请同时填写密码或授权码")
		}
	}
	return nil
}

func encryptNotificationSecret(key [32]byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), []byte(notificationAAD))
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

func decryptNotificationSecret(key [32]byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("encrypted value is too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(notificationAAD))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

type healthChange struct {
	Bookmark  bookmark
	Result    siteHealth
	Recovered bool
}

func collectHealthChanges(items []bookmark, previous, current map[string]siteHealth) []healthChange {
	changes := make([]healthChange, 0)
	for _, item := range items {
		result, exists := current[item.ID]
		if !exists {
			continue
		}
		before, hadBefore := previous[item.ID]
		isOnline := result.Status == "online"
		wasOnline := before.Status == "online"
		switch {
		case isOnline && hadBefore && !wasOnline:
			changes = append(changes, healthChange{Bookmark: item, Result: result, Recovered: true})
		case !isOnline && (!hadBefore || wasOnline):
			changes = append(changes, healthChange{Bookmark: item, Result: result})
		}
	}
	return changes
}

type notificationService struct {
	store           *notificationStore
	httpClient      *http.Client
	telegramAPIBase string
	sendMu          sync.Mutex
}

func newNotificationService(store *notificationStore) *notificationService {
	return &notificationService{
		store: store,
		httpClient: &http.Client{
			Timeout: notificationSendTimeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          4,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 8 * time.Second,
				DisableCompression:    true,
			},
		},
		telegramAPIBase: "https://api.telegram.org",
	}
}

func (s *notificationService) notifyChanges(siteName, timeZone string, changes []healthChange) {
	if len(changes) == 0 {
		return
	}
	config := s.store.get()
	if !config.TelegramEnabled && !config.EmailEnabled {
		return
	}
	subject, body := formatHealthNotification(siteName, timeZone, changes)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if config.TelegramEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), notificationSendTimeout)
		if err := s.sendTelegram(ctx, config, body); err != nil {
			fmt.Fprintf(os.Stderr, "Telegram notification failed: %v\n", err)
		}
		cancel()
	}
	if config.EmailEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), notificationSendTimeout)
		if err := sendSMTPEmail(ctx, config, subject, body); err != nil {
			fmt.Fprintf(os.Stderr, "email notification failed: %v\n", err)
		}
		cancel()
	}
}

func (s *notificationService) sendTest(ctx context.Context, channel, siteName, timeZone string) error {
	config := s.store.get()
	message := fmt.Sprintf("%s 通知测试成功\n\n自动监测服务已连接。\n发送时间：%s", siteName, formatNotificationTime(timeZone))
	switch channel {
	case "telegram":
		if config.TelegramBotToken == "" || config.TelegramChatID == "" {
			return errors.New("请先保存 Telegram Bot Token 和 Chat ID")
		}
		return s.sendTelegram(ctx, config, message)
	case "email":
		if config.SMTPHost == "" || config.SMTPFrom == "" || config.SMTPTo == "" {
			return errors.New("请先保存完整的邮箱通知设置")
		}
		if config.SMTPUsername != "" && config.SMTPPassword == "" {
			return errors.New("请先保存 SMTP 密码或授权码")
		}
		return sendSMTPEmail(ctx, config, siteName+" 通知测试", message)
	default:
		return errors.New("不支持的通知渠道")
	}
}

func formatHealthNotification(siteName, timeZone string, changes []healthChange) (string, string) {
	incidents := 0
	recoveries := 0
	for _, change := range changes {
		if change.Recovered {
			recoveries++
		} else {
			incidents++
		}
	}
	subject := fmt.Sprintf("%s：%d 个异常，%d 个恢复", siteName, incidents, recoveries)
	var body strings.Builder
	fmt.Fprintf(&body, "%s 网站状态变动\n异常：%d　恢复：%d\n", siteName, incidents, recoveries)
	limit := min(len(changes), maxNotificationChanges)
	for index := 0; index < limit; index++ {
		change := changes[index]
		if change.Recovered {
			fmt.Fprintf(&body, "\n[已恢复] %s\n%s\n延迟：%d ms\n", change.Bookmark.Title, change.Bookmark.URL, change.Result.LatencyMS)
		} else {
			reason := change.Result.Error
			if reason == "" && change.Result.HTTPCode != 0 {
				reason = "HTTP " + strconv.Itoa(change.Result.HTTPCode)
			}
			if reason == "" {
				reason = "无法访问"
			}
			fmt.Fprintf(&body, "\n[异常] %s\n%s\n原因：%s\n", change.Bookmark.Title, change.Bookmark.URL, reason)
		}
	}
	if omitted := len(changes) - limit; omitted > 0 {
		fmt.Fprintf(&body, "\n另有 %d 项变动未展开，请登录面板查看。\n", omitted)
	}
	fmt.Fprintf(&body, "\n检测时间：%s", formatNotificationTime(timeZone))
	return subject, body.String()
}

func formatNotificationTime(timeZone string) string {
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		location = time.Local
	}
	return time.Now().In(location).Format("2006-01-02 15:04:05 MST")
}

func (s *notificationService) sendTelegram(ctx context.Context, config notificationConfig, message string) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  config.TelegramChatID,
		"text":                     message,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(s.telegramAPIBase, "/") + "/bot" + config.TelegramBotToken + "/sendMessage"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.httpClient.Do(request)
	if err != nil {
		message := strings.ReplaceAll(err.Error(), config.TelegramBotToken, "***")
		return errors.New(message)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("Telegram API returned %d: %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxNotificationResponseBytes))
	return nil
}

func sendSMTPEmail(ctx context.Context, config notificationConfig, subject, body string) error {
	from, err := mail.ParseAddress(config.SMTPFrom)
	if err != nil {
		return errors.New("发件人邮箱格式无效")
	}
	recipients, err := mail.ParseAddressList(config.SMTPTo)
	if err != nil || len(recipients) == 0 {
		return errors.New("收件人邮箱格式无效")
	}
	address := net.JoinHostPort(config.SMTPHost, strconv.Itoa(config.SMTPPort))
	tlsConfig := &tls.Config{ServerName: config.SMTPHost, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: notificationSendTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("连接 SMTP 失败: %w", err)
	}
	security := config.SMTPSecurity
	if security == "auto" {
		if config.SMTPPort == 465 {
			security = "tls"
		} else {
			security = "starttls"
		}
	}
	if security == "tls" {
		tlsConnection := tls.Client(connection, tlsConfig)
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			_ = connection.Close()
			return fmt.Errorf("SMTP TLS 握手失败: %w", err)
		}
		connection = tlsConnection
	}
	_ = connection.SetDeadline(time.Now().Add(notificationSendTimeout))
	client, err := smtp.NewClient(connection, config.SMTPHost)
	if err != nil {
		_ = connection.Close()
		return fmt.Errorf("初始化 SMTP 失败: %w", err)
	}
	defer client.Close()
	if security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP 服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
		}
	}
	if config.SMTPUsername != "" {
		if err := client.Auth(smtp.PlainAuth("", config.SMTPUsername, config.SMTPPassword, config.SMTPHost)); err != nil {
			return fmt.Errorf("SMTP 登录失败: %w", err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("设置发件人失败: %w", err)
	}
	toAddresses := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient.Address); err != nil {
			return fmt.Errorf("设置收件人失败: %w", err)
		}
		toAddresses = append(toAddresses, recipient.String())
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("创建邮件内容失败: %w", err)
	}
	normalizedBody := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	message := "From: " + from.String() + "\r\n" +
		"To: " + strings.Join(toAddresses, ", ") + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" + normalizedBody + "\r\n"
	if _, err := io.WriteString(writer, message); err != nil {
		_ = writer.Close()
		return fmt.Errorf("发送邮件内容失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("SMTP 结束会话失败: %w", err)
	}
	return nil
}

type monitorScheduler struct {
	bookmarks     *store
	health        *healthManager
	notifications *notificationStore
	reset         chan struct{}
}

func newMonitorScheduler(bookmarks *store, health *healthManager, notifications *notificationStore) *monitorScheduler {
	return &monitorScheduler{
		bookmarks:     bookmarks,
		health:        health,
		notifications: notifications,
		reset:         make(chan struct{}, 1),
	}
}

func (s *monitorScheduler) run(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reset:
			resetTimer(timer, s.nextDelay())
		case <-timer.C:
			config := s.notifications.get()
			if config.MonitorEnabled {
				s.health.start(s.bookmarks.list())
			}
			resetTimer(timer, s.nextDelay())
		}
	}
}

func (s *monitorScheduler) nextDelay() time.Duration {
	config := s.notifications.get()
	if !config.MonitorEnabled {
		return 24 * time.Hour
	}
	return time.Duration(config.IntervalMinutes) * time.Minute
}

func (s *monitorScheduler) reschedule() {
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

type notificationTestInput struct {
	Channel string `json:"channel"`
}

func (a *app) handleNotificationSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.notifications.response())
}

func (a *app) handleNotificationSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input notificationInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.notifications.update(input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.scheduler != nil {
		a.scheduler.reschedule()
	}
	writeJSON(w, http.StatusOK, a.notifications.response())
}

func (a *app) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input notificationTestInput
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notificationSendTimeout)
	defer cancel()
	preferences := a.preferences.get()
	if err := a.notifier.sendTest(ctx, input.Channel, preferences.SiteName, preferences.TimeZone); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "测试通知已发送"})
}
