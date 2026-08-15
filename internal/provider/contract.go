package provider

import (
	"context"
	"errors"
	"strings"
)

type ProviderIdentity struct {
	ID          ID
	DisplayName string
	FixtureOnly bool
}

func (i ProviderIdentity) Normalize() ProviderIdentity {
	i.ID = NormalizeID(string(i.ID))
	i.DisplayName = strings.TrimSpace(i.DisplayName)
	return i
}

func (i ProviderIdentity) Validate() error {
	i = i.Normalize()
	if i.ID == "" || i.DisplayName == "" {
		return errors.New("provider identity requires id and display name")
	}
	return nil
}

type RealmRequest struct {
	ProviderID ID
	MachineID  string
	TabID      string
	ChatID     string
	// AccountScope and InstallScope are optional, non-secret identities supplied
	// by an already established lane. New lanes leave them empty and let the
	// registered resolver derive the strongest realm it can prove.
	AccountScope string
	InstallScope string
	Verified     bool
}

type RealmResolver interface {
	ResolveRealm(context.Context, RealmRequest) (Realm, error)
}

type CreateLaneRequest struct {
	Identity LaneIdentity
	Owner    AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
	// Reconcile is set only when a previously dispatched create survived in
	// Workass's durable outbox. Implementations must inspect durable native
	// ownership and exact-resume an existing binding; they must never issue a
	// second provider create call in this mode.
	Reconcile bool
	// CreateAfterCandidateAbsence is true only when the chat actor proves this
	// lane has never acquired provider-native coverage. A deferred provider may
	// use it after exact resume authoritatively reports that its saved candidate
	// never materialized. Historical lanes never set it.
	CreateAfterCandidateAbsence bool
}

type ResumeLaneRequest struct {
	Identity LaneIdentity
	Thread   ThreadRef
	Owner    AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
}

// AttachmentOwner is disposable process-routing metadata. It is deliberately
// excluded from LaneIdentity: changing a desktop tab or restarting a host must
// never create a new provider-native lineage.
type AttachmentOwner struct {
	TabID         string
	AgentOwnerKey string
}

type LaneFactory interface {
	Create(context.Context, CreateLaneRequest) (Lane, ThreadRef, error)
	Resume(context.Context, ResumeLaneRequest) (Lane, error)
}

type Attachment struct {
	ID       string
	Name     string
	MIMEType string
	Digest   string
	Size     int64
	// Ref is an immutable, daemon-resolvable content reference. Durable chat
	// state never embeds arbitrary provider payloads or renderer-local blobs.
	Ref string
}

// TurnPresentation is Workass-owned public/journal metadata. It is carried by
// the provider-neutral outbox so a daemon restart cannot change visible row or
// queue ownership, but adapters do not interpret it.
type TurnPresentation struct {
	UserMessageID      string
	AssistantMessageID string
	QueueID            string
	PromptText         string
	Title              string
	Origin             string // human | agent | internal; empty retains human compatibility.
	// StartedAt is the daemon-observed RFC3339Nano admission timestamp. It is
	// presentation metadata, not provider input, and survives restart so the
	// actor can rebuild the frozen transcript without inventing a new time.
	StartedAt string
}

type TurnInput struct {
	OperationID OperationID
	Text        string
	Attachments []Attachment
	// InitialContext is the bounded semantic Workass ledger seed attached only
	// to the first real input of a provider lane that has never consumed input.
	// It is part of that one sampling turn, not a replacement-session recovery
	// path and not the receipt-bearing ContextStrategy import operation used by
	// an already-established lane with later coverage gaps.
	InitialContext []ContextMessage
	ModelID        string
	ModeID         string
	Permission     string
	Presentation   TurnPresentation
	// CommitAdmission is the durable chat-actor boundary. A provider adapter
	// must call it after fixing the native turn identity and before publishing
	// any start/output event. Returning an error aborts publication and prompt
	// execution; it must never be treated as provider acceptance.
	CommitAdmission func(TurnAdmission) error
}

type TurnRef struct {
	OperationID OperationID
	NativeID    string
}

type TurnAdmission struct {
	Turn     TurnRef
	Accepted bool
	Consumed bool
}

type SteerInput struct {
	OperationID OperationID
	Turn        TurnRef
	Text        string
	Attachments []Attachment
}

type SteerReceipt struct {
	Turn             TurnRef
	Accepted         bool
	Consumed         bool
	AwaitConsumption bool
	Interrupted      bool
	Ambiguous        bool
}

type ReconcileRequest struct {
	OperationID OperationID
	Turn        TurnRef
}

type ReconcileResult struct {
	Turn     TurnRef
	Found    bool
	State    string
	Terminal bool
	Consumed bool
}

type PermissionDecision struct {
	OperationID OperationID
	RequestID   string
	OptionID    string
}

type PermissionReceipt struct {
	OperationID OperationID
	RequestID   string
	OptionID    string
	Accepted    bool
	Ambiguous   bool
}

type DeliveryCapabilities struct {
	StableInputIdentity bool
	LiveSteer           bool
	ConsumptionReceipt  bool
	TurnReadback        bool
}

type DeliveryStrategy interface {
	Capabilities() DeliveryCapabilities
	StartTurn(context.Context, TurnInput) (TurnAdmission, error)
	Steer(context.Context, SteerInput) (SteerReceipt, error)
	Cancel(context.Context, TurnRef) error
	Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error)
	ResolvePermission(context.Context, PermissionDecision) (PermissionReceipt, error)
}

type ContextImportMode string

const (
	ContextImportUnsupported ContextImportMode = "unsupported"
	ContextImportNonSampling ContextImportMode = "non_sampling"
)

type ContextCapabilities struct {
	ExactResume      bool
	ImportMode       ContextImportMode
	ImportReadback   bool
	IdempotentImport bool
	MaxImportEvents  int
	MaxImportBytes   int
	NativeCompaction bool
	VerifiedLineage  bool
}

// CreationCapabilities describe when a provider-native identity becomes a
// durable thread. Workass keeps a new provider candidate provisional until the
// transport proves that the first real user input reached that exact thread.
type CreationCapabilities struct {
	DeferredUntilInput bool
}

// ThreadCreationReceipt is implemented by a newly created lane when the
// ThreadRef returned by LaneFactory is still only a provider candidate. The
// chat actor keeps that candidate outside its immutable Thread field until an
// input-consumption event carries the matching durable thread receipt.
type ThreadCreationReceipt interface {
	ThreadCreationCommitted() bool
	PreviousCandidateAbsent() bool
}

type ContextMessage struct {
	EventID        string
	LedgerSequence uint64
	Role           string
	Text           string
	Result         string
	Attachments    []Attachment
	SourceLaneID   LaneID
	SourceProvider ID
	SourceModelID  string
	OperationID    OperationID
	NativeTurnID   string
	TerminalStatus string
	Inert          bool
}

type ContextImportRequest struct {
	OperationID OperationID
	From        uint64
	To          uint64
	Digest      string
	Messages    []ContextMessage
}

type ContextImportReceipt struct {
	OperationID OperationID
	From        uint64
	To          uint64
	Digest      string
	Found       bool
	Confirmed   bool
	Ambiguous   bool
}

type ContextCheckpoint struct {
	ID       string
	Coverage uint64
	Digest   string
}

type ContextStrategy interface {
	Capabilities() ContextCapabilities
	Import(context.Context, ContextImportRequest) (ContextImportReceipt, error)
	ReconcileImport(context.Context, ContextImportRequest) (ContextImportReceipt, error)
	Checkpoint(context.Context) (ContextCheckpoint, error)
}

type Lane interface {
	Identity() LaneIdentity
	Thread() ThreadRef
	Delivery() DeliveryStrategy
	Context() ContextStrategy
	Events() <-chan Event
	Detach(context.Context) error
}

// LaneAttachmentSnapshot is the typed, renderer-facing capability receipt for
// one disposable host attachment. It is committed with LaneOpened and cleared
// by LaneDetached, so session hydration never consults a manager-side cache.
// Native thread identity remains in ThreadRef; ConnectionID is routing metadata
// only and can change on exact resume.
type LaneAttachmentSnapshot struct {
	ConnectionID            string
	CWD                     string
	Agent                   string
	ProviderID              ID
	ProviderName            string
	Models                  []RuntimeModel
	CurrentModelID          string
	Modes                   []RuntimeMode
	CurrentModeID           string
	ImageSupport            bool
	CommandCatalogSupported bool
	CommandCatalog          *RuntimeCommandCatalog
}

type RuntimeModel struct {
	ID      string
	Name    string
	Efforts []string
}

type RuntimeMode struct {
	ID   string
	Name string
}

type RuntimeCommandCatalog struct {
	Commands              []RuntimeCommand
	Agents                []RuntimeAgent
	OutputStyle           string
	AvailableOutputStyles []string
	CommandsTruncated     int
	AgentsTruncated       int
	StylesTruncated       int
	AsOf                  int64
}

type RuntimeCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	Aliases      []string
}

type RuntimeAgent struct {
	Name        string
	Description string
	Model       string
}

func (s LaneAttachmentSnapshot) Clone() LaneAttachmentSnapshot {
	out := s
	out.Models = make([]RuntimeModel, len(s.Models))
	for i, model := range s.Models {
		model.Efforts = append([]string(nil), model.Efforts...)
		out.Models[i] = model
	}
	out.Modes = append([]RuntimeMode(nil), s.Modes...)
	if s.CommandCatalog != nil {
		catalog := *s.CommandCatalog
		catalog.Commands = make([]RuntimeCommand, len(s.CommandCatalog.Commands))
		for i, command := range s.CommandCatalog.Commands {
			command.Aliases = append([]string(nil), command.Aliases...)
			catalog.Commands[i] = command
		}
		catalog.Agents = append([]RuntimeAgent(nil), s.CommandCatalog.Agents...)
		catalog.AvailableOutputStyles = append([]string(nil), s.CommandCatalog.AvailableOutputStyles...)
		out.CommandCatalog = &catalog
	}
	return out
}

type LaneAttachmentSource interface {
	AttachmentSnapshot() LaneAttachmentSnapshot
}

// DurableEventDelivery is implemented by lanes whose compatibility broadcaster
// must not publish an event until the chat actor has fsynced it. It is a common
// delivery facet, not a provider-specific override. Pure/in-process fixture
// lanes may omit it and still satisfy Lane.
type DurableEventDelivery interface {
	RequireDurableEventCommits()
	AcknowledgeDurableEvent(sequence uint64, err error)
}

type CatalogModel struct {
	ID   string
	Name string
}

type MetadataSnapshot struct {
	Models []CatalogModel
	Modes  []string
}

type MetadataSource interface {
	ReadMetadata(context.Context) (MetadataSnapshot, error)
}

type UpdateInfo struct {
	Available bool
	Version   string
	Source    string
}

type UpdateStrategy interface {
	CheckUpdate(context.Context) (UpdateInfo, error)
}

// AuthenticationStrategy classifies provider-owned credential failures and
// tells Workass how the user can resolve them outside Workass. Providers own
// login and credential storage; the host owns only the common needs-login
// state transition and its retry/spawn circuit breaker.
type AuthenticationStrategy interface {
	IsAuthenticationFailure(error) bool
	LoginHint() string
}

type Definition struct {
	Identity       ProviderIdentity
	Realm          RealmResolver
	Runtime        LaneFactory
	Metadata       MetadataSource
	Update         UpdateStrategy
	Authentication AuthenticationStrategy
}

func (d Definition) Normalize() Definition {
	d.Identity = d.Identity.Normalize()
	return d
}

func (d Definition) Validate() error {
	d = d.Normalize()
	if err := d.Identity.Validate(); err != nil {
		return err
	}
	if d.Realm == nil {
		return errors.New("provider definition requires realm resolver")
	}
	if d.Runtime == nil {
		return errors.New("provider definition requires lane factory")
	}
	if d.Authentication == nil {
		return errors.New("provider definition requires authentication strategy")
	}
	return nil
}
