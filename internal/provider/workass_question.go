package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	WorkassQuestionAnswerPrefix     = "workass-question-v1:"
	WorkassQuestionResolvedOptionID = "workass-question-resolved-v1"
	WorkassQuestionTextLimit        = 1000
)

// ValidateWorkassQuestion enforces the existing permission-card bounds for
// the provider-neutral CLI question. Native SDK questions do not use this
// contract and continue through their existing parser.
func ValidateWorkassQuestion(question PermissionQuestion) error {
	if !question.WorkassTool {
		return errors.New("question is not a Workass CLI question")
	}
	if !validQuestionIdentifier(question.ID, 64) {
		return errors.New("question_id must use 1-64 letters, digits, underscores, or hyphens")
	}
	if _, err := ValidateOperationID(question.OperationID); err != nil {
		return errors.New("question requires a valid operation_id")
	}
	if strings.TrimSpace(question.Question) == "" || runeCount(question.Question) > 400 {
		return errors.New("question must contain 1-400 characters")
	}
	if runeCount(question.Header) > 40 {
		return errors.New("question header must be at most 40 characters")
	}
	if len(question.Options) < 1 || len(question.Options) > 4 {
		return errors.New("question must contain 1-4 options")
	}
	seen := make(map[string]struct{}, len(question.Options))
	for _, option := range question.Options {
		if !validQuestionIdentifier(option.ID, 64) {
			return errors.New("option ids must use 1-64 letters, digits, underscores, or hyphens")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return errors.New("question option ids must be unique")
		}
		seen[option.ID] = struct{}{}
		if strings.TrimSpace(option.Label) == "" || runeCount(option.Label) > 120 {
			return errors.New("question option labels must contain 1-120 characters")
		}
		if runeCount(option.Description) > 240 {
			return errors.New("question option descriptions must be at most 240 characters")
		}
	}
	return nil
}

// EncodeWorkassQuestionAnswer transports a structured answer through the
// frozen chat:permission-decide optionId field without changing that wire.
func EncodeWorkassQuestionAnswer(answer QuestionAnswer) (string, error) {
	encoded, err := json.Marshal(answer)
	if err != nil {
		return "", errors.New("could not encode Workass question answer")
	}
	if len(encoded) > 6144 {
		return "", errors.New("Workass question answer is too large")
	}
	return WorkassQuestionAnswerPrefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

// DecodeWorkassQuestionAnswer strictly validates the frozen-wire token against
// the exact question that owns the permission card. It rejects stale option
// ids, partial answers, unknown fields, and non-answer statuses with content.
func DecodeWorkassQuestionAnswer(value string, question PermissionQuestion) (*QuestionAnswer, error) {
	if err := ValidateWorkassQuestion(question); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(value, WorkassQuestionAnswerPrefix) || len(value) > 8192 {
		return nil, errors.New("permission decision is not a Workass question answer")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, WorkassQuestionAnswerPrefix))
	if err != nil || len(raw) == 0 || len(raw) > 6144 {
		return nil, errors.New("Workass question answer encoding is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var answer QuestionAnswer
	if err := decoder.Decode(&answer); err != nil {
		return nil, errors.New("Workass question answer JSON is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("Workass question answer contains trailing JSON")
	}
	if answer.Status != "answered" && answer.Status != "dismissed" && answer.Status != "cancelled" && answer.Status != "timed_out" {
		return nil, fmt.Errorf("unknown Workass question answer status %q", answer.Status)
	}
	if runeCount(answer.FreeText) > WorkassQuestionTextLimit || strings.ContainsRune(answer.FreeText, '\x00') {
		return nil, errors.New("Workass question free text is invalid or too long")
	}
	if answer.Reason != "" && (runeCount(answer.Reason) > 80 || strings.ContainsRune(answer.Reason, '\x00')) {
		return nil, errors.New("Workass question answer reason is invalid")
	}
	optionIDs := make(map[string]struct{}, len(question.Options))
	for _, option := range question.Options {
		optionIDs[option.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(answer.SelectedOptionIDs))
	for _, id := range answer.SelectedOptionIDs {
		if _, exists := optionIDs[id]; !exists {
			return nil, fmt.Errorf("selected option %q does not belong to this question", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, errors.New("Workass question answer repeats an option")
		}
		seen[id] = struct{}{}
	}
	if !question.MultiSelect && len(answer.SelectedOptionIDs) > 1 {
		return nil, errors.New("single-select question has multiple selected options")
	}
	if answer.FreeText != "" && !question.AllowFreeText {
		return nil, errors.New("free text is disabled for this question")
	}
	switch answer.Status {
	case "answered":
		if len(answer.SelectedOptionIDs) == 0 && strings.TrimSpace(answer.FreeText) == "" {
			return nil, errors.New("answered question must contain a selection or free text")
		}
	case "dismissed", "cancelled", "timed_out":
		if len(answer.SelectedOptionIDs) != 0 || answer.FreeText != "" {
			return nil, errors.New("non-answer question status cannot contain an answer")
		}
	}
	answer.SelectedOptionIDs = append([]string(nil), answer.SelectedOptionIDs...)
	return &answer, nil
}

func validQuestionIdentifier(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func runeCount(value string) int { return len([]rune(value)) }
