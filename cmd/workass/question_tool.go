package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

const (
	workassQuestionMutationKind   = "workass_ask_user_question"
	workassQuestionMutationMethod = "question.ask"
)

type workassQuestionCall struct {
	OperationID providercontract.OperationID
	Question    providercontract.PermissionQuestion
	Timeout     time.Duration
	Digest      string
}

type workassQuestionOptionInput struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type workassQuestionDigestInput struct {
	Version       uint32                       `json:"version"`
	QuestionID    string                       `json:"question_id"`
	Question      string                       `json:"question"`
	Header        string                       `json:"header,omitempty"`
	Options       []workassQuestionOptionInput `json:"options"`
	MultiSelect   bool                         `json:"multi_select"`
	AllowFreeText bool                         `json:"allow_free_text"`
	TimeoutMS     int                          `json:"timeout_ms,omitempty"`
}

type workassQuestionSelectedOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type workassQuestionResult struct {
	Status          string                          `json:"status"`
	QuestionID      string                          `json:"question_id"`
	SelectedOptions []workassQuestionSelectedOption `json:"selected_options"`
	FreeText        string                          `json:"free_text"`
	Reason          string                          `json:"reason,omitempty"`
	OperationID     string                          `json:"operation_id"`
}

func parseWorkassQuestion(params map[string]any) (workassQuestionCall, error) {
	allowed := map[string]struct{}{
		"question_id": {}, "question": {}, "header": {}, "options": {}, "multi_select": {},
		"allow_free_text": {}, "timeout_ms": {}, "operation_id": {},
		"parent_chat_id": {}, "parent_tab_id": {}, "owner_key": {},
	}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return workassQuestionCall{}, fmt.Errorf("unknown question argument %q", key)
		}
	}
	op, present := params["operation_id"]
	operationText, ok := op.(string)
	if !present || !ok {
		return workassQuestionCall{}, errors.New("mutating Workass question requires a caller-stable operation_id")
	}
	operationID, err := providercontract.ValidateOperationID(operationText)
	if err != nil {
		return workassQuestionCall{}, err
	}
	questionID, err := questionString(params, "question_id", true)
	if err != nil {
		return workassQuestionCall{}, err
	}
	prompt, err := questionString(params, "question", true)
	if err != nil {
		return workassQuestionCall{}, err
	}
	header, err := questionString(params, "header", false)
	if err != nil {
		return workassQuestionCall{}, err
	}
	multiSelect, err := questionBool(params, "multi_select")
	if err != nil {
		return workassQuestionCall{}, err
	}
	allowFreeText, err := questionBool(params, "allow_free_text")
	if err != nil {
		return workassQuestionCall{}, err
	}
	options, err := questionOptions(params["options"])
	if err != nil {
		return workassQuestionCall{}, err
	}
	timeoutMS := 0
	if raw, exists := params["timeout_ms"]; exists {
		timeoutMS, ok = integerQuestionValue(raw)
		if !ok || timeoutMS < 1000 || timeoutMS > 3600000 {
			return workassQuestionCall{}, errors.New("question timeout_ms must be between 1000 and 3600000")
		}
	}
	question := providercontract.PermissionQuestion{
		WorkassTool: true, ID: questionID, OperationID: string(operationID),
		Question: prompt, Header: header,
		Options:     make([]providercontract.PermissionQuestionOption, 0, len(options)),
		MultiSelect: multiSelect, AllowFreeText: allowFreeText,
	}
	for _, option := range options {
		question.Options = append(question.Options, providercontract.PermissionQuestionOption{
			ID: option.ID, Label: option.Label, Description: option.Description,
		})
	}
	if err := providercontract.ValidateWorkassQuestion(question); err != nil {
		return workassQuestionCall{}, err
	}
	// Validate caller bounds before redaction so a secret-shaped oversized value
	// cannot be accepted merely because redaction shortened its display form.
	question.Question = acp.RedactSensitiveText(question.Question)
	question.Header = acp.RedactSensitiveText(question.Header)
	for index := range question.Options {
		question.Options[index].Label = acp.RedactSensitiveText(question.Options[index].Label)
		question.Options[index].Description = acp.RedactSensitiveText(question.Options[index].Description)
	}
	if err := providercontract.ValidateWorkassQuestion(question); err != nil {
		return workassQuestionCall{}, err
	}
	// Hash the exact validated request fields, before display-only secret
	// redaction. The digest itself is safe to persist and makes same-id changes
	// detectable without placing question text in the actor receipt.
	digestInput := workassQuestionDigestInput{
		Version: 1, QuestionID: questionID, Question: prompt, Header: header, Options: options,
		MultiSelect: multiSelect, AllowFreeText: allowFreeText, TimeoutMS: timeoutMS,
	}
	digestBytes, err := json.Marshal(digestInput)
	if err != nil {
		return workassQuestionCall{}, errors.New("question request could not be normalized")
	}
	digest := sha256.Sum256(append([]byte("workass-question-v1\x00"), digestBytes...))
	return workassQuestionCall{OperationID: operationID, Question: question, Timeout: time.Duration(timeoutMS) * time.Millisecond,
		Digest: hex.EncodeToString(digest[:])}, nil
}

func questionString(params map[string]any, name string, required bool) (string, error) {
	raw, exists := params[name]
	if !exists && !required {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || required && strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("question %s must be a non-empty string", name)
	}
	return value, nil
}

func questionBool(params map[string]any, name string) (bool, error) {
	raw, exists := params[name]
	if !exists {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("question %s must be a boolean", name)
	}
	return value, nil
}

func questionOptions(raw any) ([]workassQuestionOptionInput, error) {
	var values []any
	switch items := raw.(type) {
	case []any:
		values = items
	case []map[string]any:
		values = make([]any, len(items))
		for index := range items {
			values[index] = items[index]
		}
	default:
		return nil, errors.New("question options must be an array")
	}
	if len(values) < 1 || len(values) > 4 {
		return nil, errors.New("question must contain 1-4 options")
	}
	options := make([]workassQuestionOptionInput, 0, len(values))
	for index, value := range values {
		option, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("question option %d must be an object", index+1)
		}
		for key := range option {
			if key != "id" && key != "label" && key != "description" {
				return nil, fmt.Errorf("unknown question option field %q", key)
			}
		}
		id, idOK := option["id"].(string)
		label, labelOK := option["label"].(string)
		description := ""
		if rawDescription, exists := option["description"]; exists {
			var descriptionOK bool
			description, descriptionOK = rawDescription.(string)
			if !descriptionOK {
				return nil, fmt.Errorf("question option %d description must be a string", index+1)
			}
		}
		if !idOK || !labelOK {
			return nil, fmt.Errorf("question option %d requires string id and label fields", index+1)
		}
		options = append(options, workassQuestionOptionInput{ID: id, Label: label, Description: description})
	}
	return options, nil
}

func integerQuestionValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Int64()
		return int(parsed), err == nil && int64(int(parsed)) == parsed
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		if value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}

func (r *providerChatRuntime) executeAgentQuestion(
	ctx context.Context,
	manager *acp.Manager,
	ownerKey, tabID, chatID string,
	request workassQuestionCall,
) (any, error) {
	if manager == nil {
		return nil, errors.New("Workass question manager is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	if err := lockExternalMutation(ctx, &actor.externalMutationMu); err != nil {
		return nil, err
	}
	defer actor.externalMutationMu.Unlock()

	entry, exists, err := inspectBrowserMutation(actor, tabID, chatID, request.OperationID,
		workassQuestionMutationKind, workassQuestionMutationMethod, request.Digest)
	if err != nil {
		return nil, err
	}
	if exists && entry.Status == chat.OutboxCompleted {
		return decodeWorkassQuestionReceipt(entry.Result, request.Question, request.OperationID)
	}
	if exists && entry.Status == chat.OutboxFailed {
		return nil, errors.New("Workass question operation has a durable failed receipt")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !exists || entry.Status == chat.OutboxPending {
		if err := manager.ValidateWorkassQuestionCaller(ownerKey, chatID, tabID); err != nil {
			return nil, err
		}
	}

	entry, dispatchClaimed, err := prepareBrowserMutation(actor, tabID, chatID, request.OperationID,
		workassQuestionMutationKind, workassQuestionMutationMethod, request.Digest)
	if err != nil {
		return nil, err
	}
	if dispatchClaimed {
		answer, askErr := manager.AskAgentQuestion(ctx, ownerKey, chatID, tabID, request.Question, request.Timeout)
		if askErr != nil {
			// A turn that ends between admission and card publication owns an
			// explicit cancelled receipt. The claimed operation is never replayed
			// into a second popup.
			answer = &providercontract.QuestionAnswer{Status: "cancelled", Reason: "provider_turn_ended"}
		}
		result, resultErr := workassQuestionResultFor(request.Question, request.OperationID, answer)
		if resultErr != nil {
			return nil, resultErr
		}
		if err := persistWorkassQuestionResult(actor, tabID, chatID, request, result); err != nil {
			return nil, err
		}
		return result, nil
	}

	// A dispatched retry is read-only. The actor's permission answer may have
	// committed just before the daemon or caller connection disappeared; recover
	// that exact answer if present, otherwise close the operation as cancelled.
	state := actor.engine.Snapshot()
	answer, recovered := workassQuestionAnswerForOperation(state, request.OperationID)
	if !recovered {
		answer = &providercontract.QuestionAnswer{Status: "cancelled", Reason: "operation_interrupted"}
	}
	result, err := workassQuestionResultFor(request.Question, request.OperationID, answer)
	if err != nil {
		return nil, err
	}
	if err := persistWorkassQuestionResult(actor, tabID, chatID, request, result); err != nil {
		return nil, err
	}
	return result, nil
}

func lockExternalMutation(ctx context.Context, mutex *sync.Mutex) error {
	if mutex == nil {
		return errors.New("Workass question mutation lock is unavailable")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if mutex.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func workassQuestionResultFor(question providercontract.PermissionQuestion, operationID providercontract.OperationID, answer *providercontract.QuestionAnswer) (workassQuestionResult, error) {
	if answer == nil {
		return workassQuestionResult{}, errors.New("Workass question returned no answer status")
	}
	token, err := providercontract.EncodeWorkassQuestionAnswer(*answer)
	if err != nil {
		return workassQuestionResult{}, err
	}
	answer, err = providercontract.DecodeWorkassQuestionAnswer(token, question)
	if err != nil {
		return workassQuestionResult{}, errors.New("Workass question answer failed its owning-request validation")
	}
	result := workassQuestionResult{
		Status: answer.Status, QuestionID: question.ID,
		SelectedOptions: []workassQuestionSelectedOption{}, FreeText: answer.FreeText,
		Reason: answer.Reason, OperationID: string(operationID),
	}
	options := make(map[string]string, len(question.Options))
	for _, option := range question.Options {
		options[option.ID] = option.Label
	}
	for _, id := range answer.SelectedOptionIDs {
		label, exists := options[id]
		if !exists {
			return workassQuestionResult{}, errors.New("Workass question answer contains a stale option")
		}
		result.SelectedOptions = append(result.SelectedOptions, workassQuestionSelectedOption{ID: id, Label: label})
	}
	if answer.Status != "answered" {
		result.SelectedOptions = []workassQuestionSelectedOption{}
		result.FreeText = ""
	}
	return result, nil
}

func workassQuestionAnswerForOperation(state chat.State, operationID providercontract.OperationID) (*providercontract.QuestionAnswer, bool) {
	for _, permission := range state.Permissions {
		question := permission.Event.Question
		if question == nil || !question.WorkassTool || question.OperationID != string(operationID) {
			continue
		}
		if question.Answer != nil {
			answer := *question.Answer
			answer.SelectedOptionIDs = append([]string(nil), question.Answer.SelectedOptionIDs...)
			return &answer, true
		}
		for _, entry := range state.Outbox {
			if entry.Kind != chat.EffectPermission || entry.RequestID != permission.Event.RequestID || entry.OptionID == "" {
				continue
			}
			if answer, err := providercontract.DecodeWorkassQuestionAnswer(entry.OptionID, *question); err == nil {
				return answer, true
			}
		}
	}
	return nil, false
}

func persistWorkassQuestionResult(actor *providerChatActor, tabID, chatID string, request workassQuestionCall, result workassQuestionResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return errors.New("Workass question result could not be serialized")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.engine.Apply(chat.ExternalMutationReceipt{
		OperationID: request.OperationID, Kind: workassQuestionMutationKind, Method: workassQuestionMutationMethod,
		TabID: tabID, Digest: request.Digest, Result: raw,
	})
}

func decodeWorkassQuestionReceipt(raw json.RawMessage, question providercontract.PermissionQuestion, operationID providercontract.OperationID) (any, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("Workass question has no durable answer receipt")
	}
	var result workassQuestionResult
	if err := json.Unmarshal(raw, &result); err != nil || result.QuestionID != question.ID || result.OperationID != string(operationID) ||
		(result.Status != "answered" && result.Status != "dismissed" && result.Status != "cancelled" && result.Status != "timed_out") ||
		!utf8.ValidString(result.FreeText) || utf8.RuneCountInString(result.FreeText) > providercontract.WorkassQuestionTextLimit {
		return nil, errors.New("Workass question receipt is malformed")
	}
	answer := providercontract.QuestionAnswer{Status: result.Status, FreeText: result.FreeText, Reason: result.Reason}
	for _, selected := range result.SelectedOptions {
		label := ""
		for _, option := range question.Options {
			if selected.ID == option.ID {
				label = option.Label
				break
			}
		}
		if label == "" || label != selected.Label {
			return nil, errors.New("Workass question receipt changed its selected options")
		}
		answer.SelectedOptionIDs = append(answer.SelectedOptionIDs, selected.ID)
	}
	token, err := providercontract.EncodeWorkassQuestionAnswer(answer)
	if err != nil {
		return nil, errors.New("Workass question receipt answer is invalid")
	}
	if _, err := providercontract.DecodeWorkassQuestionAnswer(token, question); err != nil {
		return nil, errors.New("Workass question receipt answer is invalid")
	}
	return result, nil
}
