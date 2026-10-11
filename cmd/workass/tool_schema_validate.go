package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// validateToolArguments checks a call's arguments against the tool's declared
// input schema before any handler runs, as Claude Code validates tool input.
// A typo such as {"deliver":"steer"} or {"forse":true} is rejected instead of
// silently running with defaults. Only the JSON Schema subset the Workass
// catalogs use is supported.
func validateToolArguments(tool map[string]any, arguments map[string]any) error {
	raw, err := json.Marshal(tool["inputSchema"])
	if err != nil {
		return fmt.Errorf("tool schema is invalid: %w", err)
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("tool schema is invalid: %w", err)
	}
	var value any = map[string]any{}
	if arguments != nil {
		value = arguments
	}
	return validateSchemaValue(schema, value, "arguments")
}

func validateSchemaValue(rawSchema, value any, path string) error {
	schema, ok := rawSchema.(map[string]any)
	if !ok {
		return nil
	}
	if alternatives, ok := schema["anyOf"].([]any); ok && len(alternatives) > 0 {
		var first error
		matched := false
		for _, alternative := range alternatives {
			if err := validateSchemaValue(alternative, value, path); err == nil {
				matched = true
				break
			} else if first == nil {
				first = err
			}
		}
		if !matched {
			return first
		}
	}
	if values, ok := schema["enum"].([]any); ok && !schemaEnumContains(values, value) {
		allowed := make([]string, 0, len(values))
		for _, item := range values {
			allowed = append(allowed, fmt.Sprint(item))
		}
		return fmt.Errorf("%s must be one of %s", path, strings.Join(allowed, ", "))
	}
	switch kind, _ := schema["type"].(string); kind {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				key, _ := name.(string)
				if _, present := object[key]; !present {
					return fmt.Errorf("%s.%s is required", path, key)
				}
			}
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			propertySchema, known := properties[key]
			if !known {
				if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
					return fmt.Errorf("%s.%s is not a known argument", path, key)
				}
				continue
			}
			if err := validateSchemaValue(propertySchema, object[key], path+"."+key); err != nil {
				return err
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if limit, ok := schemaNumber(schema["minItems"]); ok && float64(len(items)) < limit {
			return fmt.Errorf("%s needs at least %v items", path, limit)
		}
		if limit, ok := schemaNumber(schema["maxItems"]); ok && float64(len(items)) > limit {
			return fmt.Errorf("%s allows at most %v items", path, limit)
		}
		for index, item := range items {
			if err := validateSchemaValue(schema["items"], item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		length := float64(utf8.RuneCountInString(text))
		if limit, ok := schemaNumber(schema["minLength"]); ok && length < limit {
			return fmt.Errorf("%s must be at least %v characters", path, limit)
		}
		if limit, ok := schemaNumber(schema["maxLength"]); ok && length > limit {
			return fmt.Errorf("%s must be at most %v characters", path, limit)
		}
		if pattern, ok := schema["pattern"].(string); ok && pattern != "" {
			if re, err := regexp.Compile(pattern); err == nil && !re.MatchString(text) {
				return fmt.Errorf("%s has an invalid format", path)
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "integer", "number":
		number, ok := schemaNumber(value)
		if !ok {
			if kind == "integer" {
				return fmt.Errorf("%s must be an integer", path)
			}
			return fmt.Errorf("%s must be a number", path)
		}
		if kind == "integer" && number != math.Trunc(number) {
			return fmt.Errorf("%s must be an integer", path)
		}
		if limit, ok := schemaNumber(schema["minimum"]); ok && number < limit {
			return fmt.Errorf("%s must be at least %v", path, limit)
		}
		if limit, ok := schemaNumber(schema["maximum"]); ok && number > limit {
			return fmt.Errorf("%s must be at most %v", path, limit)
		}
	}
	return nil
}

func schemaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	case float64:
		return number, true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	}
	return 0, false
}

func schemaEnumContains(values []any, value any) bool {
	for _, candidate := range values {
		if left, ok := schemaNumber(candidate); ok {
			if right, ok := schemaNumber(value); ok && left == right {
				return true
			}
			continue
		}
		if candidate == value {
			return true
		}
	}
	return false
}
