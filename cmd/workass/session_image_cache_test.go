package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSessionImageNameMemoIsByteBoundedAndNotPayloadKeyed(t *testing.T) {
	const payloadSize = 9 << 20
	for index := 0; index < 8; index++ {
		sessionImageName(strings.Repeat(string(rune('a'+index)), payloadSize))
	}

	sessionImageNameMu.Lock()
	defer sessionImageNameMu.Unlock()
	memoType := reflect.TypeOf(sessionImageNameMemo)
	if memoType.Kind() == reflect.Map && memoType.Key().Kind() == reflect.String {
		t.Fatal("session image memo still keys its long-lived map on complete payload strings")
	}
	retained := reflectedStringBytes(reflect.ValueOf(sessionImageNameMemo), map[uintptr]struct{}{})
	if retained > 64<<20 {
		t.Fatalf("session image memo retains %d string bytes, want at most 64 MiB", retained)
	}
}

func reflectedStringBytes(value reflect.Value, seen map[uintptr]struct{}) int {
	if !value.IsValid() {
		return 0
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return 0
		}
		return reflectedStringBytes(value.Elem(), seen)
	case reflect.Pointer:
		if value.IsNil() {
			return 0
		}
		pointer := value.Pointer()
		if _, ok := seen[pointer]; ok {
			return 0
		}
		seen[pointer] = struct{}{}
		return reflectedStringBytes(value.Elem(), seen)
	case reflect.String:
		return value.Len()
	case reflect.Map:
		total := 0
		iter := value.MapRange()
		for iter.Next() {
			total += reflectedStringBytes(iter.Key(), seen)
			total += reflectedStringBytes(iter.Value(), seen)
		}
		return total
	case reflect.Slice, reflect.Array:
		total := 0
		for index := 0; index < value.Len(); index++ {
			total += reflectedStringBytes(value.Index(index), seen)
		}
		return total
	case reflect.Struct:
		total := 0
		for index := 0; index < value.NumField(); index++ {
			total += reflectedStringBytes(value.Field(index), seen)
		}
		return total
	default:
		return 0
	}
}
