package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/caarlos0/env/v11"
)

type RelayMode string

const (
	ModePublicMemory  RelayMode = "public-memory"
	ModePrivateSQLite RelayMode = "private-sqlite"
)

type Config struct {
	Host           string    `env:"NOSTR_RELAY_HOST" envDefault:"0.0.0.0"`
	Port           string    `env:"NOSTR_RELAY_PORT" envDefault:"8080"`
	ManagementHost string    `env:"NOSTR_RELAY_MANAGEMENT_HOST" envDefault:"0.0.0.0"`
	ManagementPort string    `env:"NOSTR_RELAY_MANAGEMENT_PORT" envDefault:"8081"`
	Name           string    `env:"NOSTR_RELAY_NAME" envDefault:"mytools relay"`
	Description    string    `env:"NOSTR_RELAY_DESCRIPTION" envDefault:"A minimal Nostr relay"`
	MaxQueryLimit  int       `env:"NOSTR_RELAY_MAX_QUERY_LIMIT" envDefault:"500"`
	LogLevel       string    `env:"LOG_LEVEL" envDefault:"info"`
	Mode           RelayMode `env:"NOSTR_RELAY_MODE" envDefault:"public-memory"`
	DatabasePath   string    `env:"NOSTR_RELAY_DATABASE_PATH"`
	ServiceURL     string    `env:"NOSTR_RELAY_SERVICE_URL"`
	AdminPubkey    string    `env:"NOSTR_RELAY_ADMIN_PUBKEY"`
	ReaderPubkeys  []string  `env:"NOSTR_RELAY_READER_PUBKEYS" envSeparator:","`
}

func LoadConfig() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg *Config) validate() error {
	port, err := validateListenerPort("NOSTR_RELAY_PORT", cfg.Port)
	if err != nil {
		return err
	}
	managementPort, err := validateListenerPort("NOSTR_RELAY_MANAGEMENT_PORT", cfg.ManagementPort)
	if err != nil {
		return err
	}
	host, err := validateListenerHost("NOSTR_RELAY_HOST", cfg.Host)
	if err != nil {
		return err
	}
	managementHost, err := validateListenerHost("NOSTR_RELAY_MANAGEMENT_HOST", cfg.ManagementHost)
	if err != nil {
		return err
	}
	validated := *cfg
	validated.Host = host
	validated.ManagementHost = managementHost
	switch cfg.Mode {
	case ModePublicMemory:
		// No mode-specific validation is required.
	case ModePrivateSQLite:
		if err := validated.validatePrivateSQLite(port, managementPort); err != nil {
			return err
		}
	default:
		return fmt.Errorf("NOSTR_RELAY_MODE must be %q or %q", ModePublicMemory, ModePrivateSQLite)
	}
	cfg.Host = host
	cfg.ManagementHost = managementHost
	return nil
}

func (cfg Config) validatePrivateSQLite(port, managementPort int) error {
	if port == managementPort && listenerHostsConflict(cfg.Host, cfg.ManagementHost) {
		return fmt.Errorf("NOSTR_RELAY_MANAGEMENT_HOST and NOSTR_RELAY_MANAGEMENT_PORT must select a listener distinct from NOSTR_RELAY_HOST and NOSTR_RELAY_PORT in %s mode", ModePrivateSQLite)
	}
	if cfg.DatabasePath == "" {
		return fmt.Errorf("NOSTR_RELAY_DATABASE_PATH is required in %s mode", ModePrivateSQLite)
	}
	serviceURL, err := url.Parse(cfg.ServiceURL)
	if err != nil || !serviceURL.IsAbs() || serviceURL.Host == "" || serviceURL.User != nil || serviceURL.Fragment != "" || (serviceURL.Scheme != "http" && serviceURL.Scheme != "https") {
		return fmt.Errorf("NOSTR_RELAY_SERVICE_URL must be an absolute http(s) URL in %s mode", ModePrivateSQLite)
	}
	if !validPubkey(cfg.AdminPubkey) {
		return fmt.Errorf("NOSTR_RELAY_ADMIN_PUBKEY must be 64 hexadecimal characters in %s mode", ModePrivateSQLite)
	}
	if len(cfg.ReaderPubkeys) == 0 {
		return fmt.Errorf("NOSTR_RELAY_READER_PUBKEYS requires at least one pubkey in %s mode", ModePrivateSQLite)
	}
	for _, reader := range cfg.ReaderPubkeys {
		if !validPubkey(reader) {
			return fmt.Errorf("NOSTR_RELAY_READER_PUBKEYS must contain only 64-character hexadecimal pubkeys")
		}
		if strings.EqualFold(reader, cfg.AdminPubkey) {
			return fmt.Errorf("NOSTR_RELAY_READER_PUBKEYS must not include NOSTR_RELAY_ADMIN_PUBKEY")
		}
	}
	return nil
}

func validateListenerPort(name, value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must be a decimal port number from 1 through 65535", name)
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("%s must be a decimal port number from 1 through 65535", name)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be a decimal port number from 1 through 65535", name)
	}
	return port, nil
}

func validateListenerHost(name, host string) (string, error) {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return "", fmt.Errorf("%s must be a non-empty hostname or IP address", name)
	}
	bracketed := strings.HasPrefix(trimmed, "[") || strings.HasSuffix(trimmed, "]")
	if bracketed {
		if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
			return "", fmt.Errorf("%s must use matching brackets only around an IPv6 address", name)
		}
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	if strings.Contains(trimmed, "%") {
		// netip permits arbitrary zone text. Listener zones are configuration
		// identifiers, so reject invisible characters and ambiguous delimiters
		// explicitly rather than silently normalizing them.
		if host != strings.TrimSpace(host) || !validIPv6Zone(trimmed) {
			return "", fmt.Errorf("%s must contain a valid IPv6 address when using colons, a zone, or brackets", name)
		}
	}
	if bracketed || strings.ContainsAny(trimmed, ":%") {
		addr, err := netip.ParseAddr(trimmed)
		if err != nil || !addr.Is6() {
			return "", fmt.Errorf("%s must contain a valid IPv6 address when using colons, a zone, or brackets", name)
		}
		return addr.String(), nil
	}
	return trimmed, nil
}

func validIPv6Zone(host string) bool {
	_, zone, found := strings.Cut(host, "%")
	if !found || zone == "" || strings.Contains(zone, "%") {
		return false
	}
	for _, r := range zone {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// addressHost accepts both the bare IPv6 form used by net.JoinHostPort and
// the bracketed form commonly copied from URLs. It leaves other hosts intact.
func addressHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']' {
		inner := trimmed[1 : len(trimmed)-1]
		ipLiteral := inner
		if percent := strings.LastIndexByte(ipLiteral, '%'); percent >= 0 {
			ipLiteral = ipLiteral[:percent]
		}
		if strings.Contains(ipLiteral, ":") && net.ParseIP(ipLiteral) != nil {
			return inner
		}
	}
	return trimmed
}

func listenerHostsConflict(left, right string) bool {
	left = normalizeListenerHost(left)
	right = normalizeListenerHost(right)
	leftIP, leftIsIP := parseListenerIP(left)
	rightIP, rightIsIP := parseListenerIP(right)
	if leftIsIP && rightIsIP {
		return leftIP.Equal(rightIP) || leftIP.IsUnspecified() || rightIP.IsUnspecified()
	}
	// Hostname resolution can change between validation and bind (and two names
	// may resolve to the same address), so a shared port is only provably safe
	// when both hosts are distinct, specific IP literals.
	return true
}

func normalizeListenerHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimSuffix(host, ".")
}

func parseListenerIP(host string) (net.IP, bool) {
	ip := net.ParseIP(host)
	return ip, ip != nil
}

func validPubkey(pubkey string) bool {
	if len(pubkey) != 64 {
		return false
	}
	_, err := hex.DecodeString(pubkey)
	return err == nil
}
