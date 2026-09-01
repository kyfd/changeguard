package runtimeconfig

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

func rejectShadowOnPrimary() error {
	shadow := strings.TrimSpace(os.Getenv("DBGUARD_SHADOW_DSN"))
	primary := strings.TrimSpace(os.Getenv("DBGUARD_PRIMARY_DSN"))
	if shadow == "" || primary == "" {
		return nil
	}
	shadowHost, err := databaseHostPort(shadow)
	if err != nil {
		return err
	}
	primaryHost, err := databaseHostPort(primary)
	if err != nil {
		return err
	}
	if strings.EqualFold(shadowHost, primaryHost) {
		return errors.New("CHANGEGUARD_SHADOW_DSN must not use the same host:port as CHANGEGUARD_PRIMARY_DSN")
	}
	return nil
}

func databaseHostPort(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", errors.New("database DSN must be a valid absolute URL")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	if host == "" {
		return "", errors.New("database DSN must include a host")
	}
	return net.JoinHostPort(host, port), nil
}
