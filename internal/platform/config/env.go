// Copyright 2026 Henry Zektser.

package config

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is prepended to every environment variable name.
const EnvPrefix = "MCPDOLL_"

// applyEnv overlays environment variables onto cfg.
//
// The mapping is derived from the yaml tags by reflection rather than written
// out by hand, so adding a config field cannot silently forget to add its
// environment override — the class of bug where a deployment sets
// MCPDOLL_SOMETHING and nothing happens.
func applyEnv(cfg *Config) error {
	return walk(reflect.ValueOf(cfg).Elem(), EnvPrefix)
}

func walk(v reflect.Value, prefix string) error {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.ToUpper(strings.ReplaceAll(strings.Split(tag, ",")[0], "-", "_"))
		key := prefix + name
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(time.Duration(0)) {
			if err := walk(fv, key+"_"); err != nil {
				return err
			}
			continue
		}

		raw, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if err := assign(fv, raw); err != nil {
			return fmt.Errorf("config: %s=%q: %w", key, raw, err)
		}
	}
	return nil
}

func assign(fv reflect.Value, raw string) error {
	// time.Duration is an int64 underneath, so it must be handled before the
	// integer case or "30s" would fail to parse.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("not a duration (try \"30s\", \"5m\"): %w", err)
		}
		fv.SetInt(int64(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not a boolean: %w", err)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("not an integer: %w", err)
		}
		if fv.OverflowInt(n) {
			return fmt.Errorf("%d overflows %s", n, fv.Type())
		}
		fv.SetInt(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("not a number: %w", err)
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", fv.Type().Elem())
		}
		// Comma-separated, whitespace trimmed, empties dropped — so a trailing
		// comma in a Helm value does not become an empty trusted key.
		var out []string
		for part := range strings.SplitSeq(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		fv.Set(reflect.ValueOf(out))
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}

// ParseLevel maps a configured level name to slog's level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log.level %q must be one of debug, info, warn, error", name)
	}
}
