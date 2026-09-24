#!/usr/bin/env node
import { spawn as nodeSpawn } from 'node:child_process';
import { createWriteStream } from 'node:fs';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { pathToFileURL } from 'node:url';
import { randomUUID } from 'node:crypto';

const HINTS = new Map([
  ["TestAcceptedImageNeverSilentlyDegradesToTextForUnsupportedProvider", 0],
  ["TestAcceptedImageReachesGenericProviderWithExplicitAttachmentContextOnEveryTurn", 0.03],
  ["TestAccountPlanUsageWithNativeToolsNeedsNoChatOwner", 0.03],
  ["TestACPCatalogDiscoversEffortBeforeFirstPrompt", 0.23],
  ["TestACPEffortSelectionBeforeAxisDiscovery", 1.3],
  ["TestActivityReadsDoNotCopyHistoryAndStillRejectWrongOwner", 0.01],
  ["TestActorDeleteCrashRecoveryCompletesNativeCleanupFromTombstone", 0.23],
  ["TestActorEnvironmentProjectionSurvivesRuntimeRestartAndRejectsWrongTab", 0.05],
  ["TestActorFileStoreRetainsContentAddressedProviderAttachmentAcrossRestart", 0.05],
  ["TestActorForegroundRunningExcludesLegacyUncertainState", 0],
  ["TestActorGlobalChatOrderIsBoundedAndSanitized", 0],
  ["TestActorHistoryBudgetCountsRepeatedImageOccurrencesBeforeHydration", 0],
  ["TestActorHistoryBudgetKeepsUncommittedForegroundOwnersTogether", 0],
  ["TestActorHistoryProjectionMaterializesOnlyRequestedTail", 0],
  ["TestActorManagedStartDoesNotInheritManagerBusyState", 0.02],
  ["TestActorNativeChatProjectsAfterRestartFromCanonicalStorage", 0.21],
  ["TestActorRendererSessionSnapshotParityAcrossRestart", 0.43],
  ["TestActorSpawnedWorkProjectionPreservesAcceptedRunningAndSharedTabRows", 0.03],
  ["TestAdapterAuthoredModelUpdateStillEntersActorRefresh", 0.08],
  ["TestAdapterSessionRefreshCannotOverwriteActorFromStaleAttachment", 0.19],
  ["TestAdoptedHarnessTurnEndsWhenEngineDies", 0],
  ["TestAdoptedHarnessTurnStartsAndEnds", 0],
  ["TestAgentChatSendQueuedOwnershipAndImmutableRetries", 0.17],
  ["TestAgentChatSendSteerDerivedOperationRejectsChangedMessageAndDelivery", 0.3],
  ["TestAgentControlCodexOwnerCanRegisterExternalHandoff", 0.43],
  ["TestAgentControlCreatedChatSurvivesStaleSessionSaveBeforeSend", 0.17],
  ["TestAgentControlHostsArtifactsOnlyFromTheCallingAgentWorkspace", 0.16],
  ["TestAgentControlInvalidOwnerKeepsSubagentOwnershipError", 0.05],
  ["TestAgentControlIsInProcessAndCreatesNoLegacyDescriptor", 0.01],
  ["TestAgentControlRejectsDeletedActorBeforeLiveManagerAuthorization", 0.15],
  ["TestAgentControlRejectsOutOfRangeWaitTimeout", 0],
  ["TestAgentControlTurnlessSpawnNeverUsesLegacySessionMirrorAsOwner", 0.11],
  ["TestAgentMCPRedactsReflectedToolErrors", 0],
  ["TestAgentMCPRejectsLegacyArgumentAliases", 0],
  ["TestAgentMCPToolCallsDirectInProcessControl", 0.1],
  ["TestAgentMCPToolCatalogKeepsTypedSubagentContract", 0],
  ["TestAgentMessageChunkPreservesStructuredACPImages", 0],
  ["TestAgentMessageMarkdownImageStreamsBeforeTurnEndAcrossSplitChunks", 0],
  ["TestAgentOwnerCapabilityCannotBeRetargetedToAnotherChat", 0],
  ["TestAgentOwnerCWDFallsBackToManagerRootForTurnlessOwner", 0],
  ["TestAgentOwnerCWDUsesExactOwnedRunningWorkspace", 0],
  ["TestAgentStatelessMCPFencesDeletedActorBeforeOwnerValidation", 0.3],
  ["TestAgentUpdateWireFencesAuthorizedApplyBeforeRenderer", 0],
  ["TestAgentUpdateWireRoutesOnlyTheExactMachine", 0],
  ["TestApplyProductionDefaultsWindowsOnlyAndPreservesOverrides", 0],
  ["TestAppMetaAdvertisesCapabilityGatedLeanSessionSaves", 0],
  ["TestAppMetaDefaultsUnknownRuntimeProfilesToProduction", 0],
  ["TestArchiveWirePagesBeforeStableActorMessageWithoutChangingLegacyFullRead", 0],
  ["TestArtifactTransferWirePairedRoundTripAndUnpairedReject", 0.02],
  ["TestArtifactValidationFailureIsTerminalAndRetryDoesNotInspectSource", 0.17],
  ["TestAssistantImagesDoNotRereadImportedFilesAndRetryMissingAtTerminal", 0],
  ["TestAttachedImageAndContextSurviveNativeSteeringForEveryFrontierProvider", 0.06],
  ["TestAuthenticatedProvidersShareProviderContractStrategy", 0],
  ["TestAuthenticationDemotionRedactsReturnedAndCatalogErrors", 0.01],
  ["TestAuthoritativeAvailableModelsReplaceRenamedAddedAndRemovedModels", 0],
  ["TestAuthShapedErrorDoesNotMatchAuthSubstringInsideOrdinaryWords", 0],
  ["TestBeginUpdateDrainLatchesAdmissionUntilProcessExit", 0],
  ["TestBlockedOnTheUserIsReadFromTheNativeSignal", 0],
  ["TestBoundedActorSnapshotPreservesCanonicalHistoryCount", 0],
  ["TestBrandForProvider", 0],
  ["TestBridgeCloseOrphansInProcessSpawnedWork", 0.04],
  ["TestBrowserMCPListsToolsAndRoutesProviderNeutralCalls", 0],
  ["TestBrowserMCPMutationCarriesOperationIdentityAndDigest", 0],
  ["TestBrowserMCPRejectsLegacyCamelOperationID", 0],
  ["TestBrowserMCPRejectsUnsupportedOrInvalidFieldsBeforeBrowserDispatch", 0],
  ["TestBrowserMCPReturnsToolErrorWhenBrowserIsUnavailable", 0],
  ["TestBrowserMCPTargetValidationTreatsNullSelectorAsSupplied", 0],
  ["TestBrowserMCPTypePreservesEmptyAndWhitespaceReplacementText", 0],
  ["TestBrowserMCPViewportAndNestedBatchArgumentsMapOnlySupportedNames", 0],
  ["TestBrowserMutationReleasesActorLockAndSerializesConcurrentRetry", 0],
  ["TestBrowserReadReleasesActorLockDuringShellHTTP", 0.34],
  ["TestBrowserStatelessMCPMutationJournalReadbackConflictAndActorFence", 0.6],
  ["TestBrowserStatelessMCPRejectsLiveManagerOwnerAfterActorDeletion", 0.3],
  ["TestBrowserStatelessMCPUnreadyControlDoesNotClaimActorMutation", 0.42],
  ["TestBuiltInFrontierProvidersUseOfficialNativeCommands", 0],
  ["TestBuiltInOpenCodeProviderDefaultsToOxAlphaFree", 0],
  ["TestCancelDuringCommitAdmissionStartsNoProviderPrompt", 0.05],
  ["TestCancelInPromptPreparationGap", 1.35],
  ["TestCancelledCatalogRefreshWaiterDoesNotRecurse", 0],
  ["TestCancellingASessionSettlesAParkedQuestion", 0],
  ["TestCatalogModelSelectionBaseNormalizesOnlyCanonicalSuffixesOnExistingModels", 0],
  ["TestCatalogProbeDoesNotReceiveOwnerInstructions", 0],
  ["TestCatalogReadExpiryRefreshesRemoteModelsWithoutCLIVersionChange", 0.05],
  ["TestCatalogRefreshAuthenticationFailurePreservesUsableLastGoodCatalog", 0.01],
  ["TestChatCheckpointRotationAndLargeRepoSkip", 0.45],
  ["TestChatCheckpointsDiffRewindAndOutsideGuard", 0.63],
  ["TestChatCommandsGetIsUnsupportedForUnknownChatsAndUnadvertisedHosts", 0.02],
  ["TestChatControlIdleCancelReceiptIsDurableBeforeRetry", 0.03],
  ["TestChatControlInvalidOperationCannotCancelOrDeleteRunningTurn", 0.42],
  ["TestChatControlMutationPreflightRejectsBeforeManagerSideEffects", 0.03],
  ["TestChatControlRefreshUsesExactActorRevisionWithoutSessionMirror", 0.04],
  ["TestChatControlRenameReceiptIsStableAcrossLostReply", 0.03],
  ["TestChatControlVisibleMutationRefreshesAreImmediate", 0.34],
  ["TestChatDiagnosticsExactActorAndNoMutation", 0.38],
  ["TestChatDiagnosticsToolRemoteRoute", 0.42],
  ["TestChatEnvNonGitCwdIsEmpty", 0.02],
  ["TestChatEnvTracksRepoChangesAfterTurn", 0.45],
  ["TestChatEnvTruncationFlags", 0.95],
  ["TestChatLifecycleDoesNotRunAutomaticGit", 1.54],
  ["TestChatLifecycleDoesNotRunAutomaticGitCase", 0],
  ["TestChatListToolPreservesLocalChatsWithMountedRemote", 0.44],
  ["TestChatWorkspaceForExactPairRejectsStaleTab", 0.07],
  ["TestCheckpointLoaderRejectsUnversionedAndUnownedState", 0],
  ["TestCheckpointRestorePublishesOnlyAfterEnvironmentActorCommit", 0.05],
  ["TestClampDemotesDoneOnHarnessParkEvidence", 0],
  ["TestClaudeAnsweredQuestionRowCompletesAndAnUnansweredOneStillFails", 0.13],
  ["TestClaudeBridgeLaunchUsesWorkassSDKHostAndOfficialExecutable", 0],
  ["TestClaudeCatalogProbeDiscoversEffortBehindDefaultAlias", 0.01],
  ["TestClaudeCommandCatalogIsGatedToTheClaudeProvider", 0.27],
  ["TestClaudeCommandCatalogReclampsASkewedHostPayload", 0.02],
  ["TestClaudeCommandCatalogRidesOpenReplyEmitsEventAndAnswersCommandsGet", 0.02],
  ["TestClaudeCommandCatalogSurvivesHibernationAsCachedSnapshot", 0.05],
  ["TestClaudeEffortAxisSurfacesAndRoutesSeparately", 0.07],
  ["TestClaudeEffortCapabilityStaysModelSpecificWhenSwitchingToHaiku", 0.08],
  ["TestClaudeNativeQuestionReachesTheClientAndItsAnswerReachesTheModel", 0.06],
  ["TestClaudeNativeSteerUsesAcknowledgedLiveRequestWithoutCancellingRunningTurn", 0.03],
  ["TestClaudeNativeTurnRecoversTransientOAuthRefreshWithoutFalseAssistantAnswer", 0.05],
  ["TestClaudeNotificationAdapterConsumesMalformedPrivateFramesSafely", 0],
  ["TestClaudeNotificationAdapterProducesTypedLifecycleEvents", 0],
  ["TestClaudePassiveSyntheticDefaultWinsAsExplicitAlias", 0.07],
  ["TestClaudeSyntheticDefaultInitialSessionUsesExplicitModel", 0.02],
  ["TestClaudeUpdateReresolvesTransientShimAndAtomicInstallSwap", 2.64],
  ["TestClaudeVerifiedLineageCommitsActorBeforeNativeMaterializationAndExactResume", 0.08],
  ["TestClaudeWithoutLiveSteerRejectsWithoutQueueingOrInterrupting", 0.02],
  ["TestCleanYieldIsNeverReadAsDone", 0],
  ["TestCloseSessionEmitsProcChanged", 0.06],
  ["TestCodexBridgeLaunchUsesWorkassAppServerHostAndOfficialExecutable", 0],
  ["TestCodexEarnedRateLimitResetIsIdempotentAndRefreshesPlanUsage", 0.02],
  ["TestCodexEarnedRateLimitResetWithoutLiveSessionUsesEphemeralFallback", 0.01],
  ["TestCodexNativeGoalThroughActorAndExactResume", 1.29],
  ["TestCodexNativeSteerRejectionDoesNotQueueOrInterrupt", 0.03],
  ["TestCodexNativeSubagentMetadataUsesExistingToolProjection", 0],
  ["TestCodexRuntimeDiagnosticsThroughActorAndRestart", 0.48],
  ["TestCodexServiceTierSurvivesExactResumeAndClearsExplicitly", 1.07],
  ["TestCodexSteerAlreadyFinishedNativeTurnRejectsWithoutInterruptingWrapper", 0.04],
  ["TestCodexSteerDuplicateConsumptionReceiptIsIdempotent", 0.03],
  ["TestCodexSteerNonSteerableReviewRejectsWithoutQueueingOrCancellingReview", 0.24],
  ["TestCodexSteerUsesAcknowledgedNativeRequestWithoutCancellingRunningTurn", 0.04],
  ["TestCommitAdmissionFailureDropsReservationBeforeProviderPrompt", 0.05],
  ["TestCommitSpawnedWorkChangeAdvancesBridgeLastActivity", 0.02],
  ["TestCompatibleModeIDTranslatesProviderPermissionIntent", 0],
  ["TestCompletedAppChatJobsArePruned", 0.02],
  ["TestCompletedTurnIsNotMarkedInterrupted", 0.05],
  ["TestCompositeModelCreateValidation", 0.16],
  ["TestConfigAndSettingsPersistInStateDir", 0.01],
  ["TestConfigureChatReplaysWorkspaceAndControlsReceiptsAsOneRecoverableAction", 0.15],
  ["TestConfiguredBrowserPromptIsAdjacentToEveryTopLevelTurn", 0],
  ["TestConfiguredProviderUsesUnifiedRegistryAndExactLaneFactory", 0.05],
  ["TestContextDeltaPromptPreservesSessionAndCurrentRequestBoundary", 0],
  ["TestContextDeltaTraversesRuntimeIntoExactResumedMockThread", 0.15],
  ["TestCoordinatedSubagentUsesExplicitSelectionRoutesPermissionAndNamespacesTools", 0.25],
  ["TestCoordinatorRetiresOldAttachmentBeforeExactResume", 0.14],
  ["TestCorruptNativeSessionLedgerDisablesResumeWithoutOverwriting", 0],
  ["TestCrossProviderSelectionFailsBeforeDetachingActiveLane", 0.02],
  ["TestDaemonEventBroadcasterPreservesPublicationOrder", 0],
  ["TestDaemonIdentityDescribesTheMachineAndWhatItSpeaks", 0],
  ["TestDaemonIdentityOmitsAnUnsetProfileRatherThanGuessing", 0],
  ["TestDaemonIdentityOmitsTheIDWhenIdentityIsUnavailable", 0],
  ["TestDaemonMetricsUsesAuthoritativeActorInventory", 0.01],
  ["TestDaemonRestartDefersRatherThanVerdicts", 0],
  ["TestDaemonToolsEntrypointDispatchesBeforeStartup", 0.03],
  ["TestDecideSubagentPermissionRejectsUnknownRunWithAReason", 0],
  ["TestDeclaredServiceIsCarriedFromRegistrationAndSurvivesRestart", 0.02],
  ["TestDeclaredWorkIsNeverReclassifiedByInference", 0.65],
  ["TestDefaultBrowserControlFileEnvironmentOverrideWins", 0],
  ["TestDefaultBrowserControlFileUsesStateDirectoryProfile", 0],
  ["TestDeferredCandidateRoundTripAndThreadCommitAreAtomic", 0.01],
  ["TestDeferredCodexCreatesAgainOnlyForAProvablyEmptyLane", 0.56],
  ["TestDeferredDevinCandidateAbsencePreservesCommittedThreadProtection", 0.42],
  ["TestDeferredProviderStartupKeepsSpareSessionsStoppedUntilRelease", 0],
  ["TestDeletedActorDoesNotBlockStartupReconciliation", 0.09],
  ["TestDeletedActorRejectsOriginalCreateReplay", 0.11],
  ["TestDeliveryStrategyProjectsNegotiatedSteerSemantics", 0],
  ["TestDescriptorOnlyACPProviderInheritsCompleteGenericLaneContract", 0.2],
  ["TestDescriptorOnlyACPProviderRejectsMissingExactAttachmentBeforeCreate", 0.05],
  ["TestDetectClaudeUsesOfficialSDKSessionNotSeparateCLIAuthPreflight", 0.06],
  ["TestDetectFrontierProvidersNeedsLogin", 1.25],
  ["TestDetectFrontierProvidersReadyWithNativeProtocolFixtures", 1.26],
  ["TestDetectProvidersExplicitDisableSurvivesRedetection", 0.9],
  ["TestDetectProvidersLocalServerBinaryUnresolvableStatusError", 0.05],
  ["TestDetectProvidersLocalServerInactiveWhenDown", 0.05],
  ["TestDetectProvidersLocalServerRegistersNativeProviderAndStreamsThroughAgent", 1.93],
  ["TestDetectProvidersMissingBinaryNotFound", 0],
  ["TestDetectProvidersOMLXAuthenticatesQwenAndNativeProviderWithoutPersistingKey", 1.49],
  ["TestDetectProvidersPersistsQwenCLIPathWhenModelServerIsInactive", 0],
  ["TestDetectProvidersQwenInactiveWhenModelServerDown", 0],
  ["TestDetectProviderStoresRedactedCLIVersionRaw", 1.26],
  ["TestDevelopmentRuntimeRetainsFixtureModels", 0],
  ["TestDevinAuthenticationFailureBecomesNeedsLoginWithoutRetryLoop", 1.29],
  ["TestDevinLaunchOwnsACPBackendSanitization", 0],
  ["TestDevinObservedHandshakeDoesNotAdvertiseLiveSteering", 0],
  ["TestDevinSessionAuthenticationFailureDemotesOnceWithoutReplacement", 0.01],
  ["TestDevinStopAndSendUsesDurableQueueAndExactCancellation", 0.57],
  ["TestDispositionIsOmittedUntilThereIsOne", 0],
  ["TestDreamSubagentCatalogProgressMessageWaitManyAndDurableReceipt", 0.4],
  ["TestEarnedRateLimitResetRejectsProvidersWithoutNativeCapability", 0.02],
  ["TestEmitToolEventForwardsSubagentLinkage", 0],
  ["TestEmitToolEventPreservesVisibleRasterResults", 0],
  ["TestEngineCrashMidTurnDoesNotDisableProviderAsNeedsLogin", 0.02],
  ["TestEnvironmentBriefAdvertisesAgentCatalogAndSpawnTools", 0],
  ["TestEnvironmentBriefCurrentRequestLanguageUsesHumanRequest", 0],
  ["TestEnvironmentBriefIncludesActiveModelOnEveryTurn", 0],
  ["TestEnvironmentBriefIncludesChatArchivePath", 0],
  ["TestEnvironmentBriefOmitsDelegationAndResponseStyleDirectives", 0],
  ["TestExactAttachmentAbsenceIsProviderOwned", 0],
  ["TestExactAttachmentDoesNotTryLoadAfterSelectedResumeFails", 0.12],
  ["TestExactSessionAttachmentCapabilityMatrix", 0],
  ["TestExitPlanModeRecognitionSurvivesNamingShapes", 0],
  ["TestExplicitChatDeletionStillCancelsAdoptedSubagents", 0],
  ["TestExternalWorkRegistrationIsProviderNeutralAndSettlementSurvivesRestart", 0.08],
  ["TestFailedDetectionDisablesPreviouslyReadyProviderWithoutUserDisable", 0.86],
  ["TestFailedListenProbeNeverClassifies", 0.01],
  ["TestFailedSpareWarmTripsCircuitBreakerInsteadOfRespawning", 0.16],
  ["TestFailedTurnNeedsTheUserAndNeverReadsAsDone", 0],
  ["TestFailureNoteIsRedactedAndBounded", 0],
  ["TestFakeACPHelper", 0],
  ["TestFakeInitTimeout", 0.05],
  ["TestFakePermissionDecideRoundTrip", 0.02],
  ["TestFakePermissionTimeoutUsesFallbackDeny", 0.08],
  ["TestFakePromptSerializationQueuesPerBridge", 0.17],
  ["TestFakeStderrTailCapturedOnCrash", 0.01],
  ["TestFirstInputInitialContextSeedIsIncludedOnce", 0],
  ["TestFleetCLIMintsJoinsListsAndForgets", 0],
  ["TestFleetCLIRefusesGarbageAndNamesTheCommandOrder", 0],
  ["TestFleetQRDrawsFromAnExistingKeyAndNamesItsSource", 0],
  ["TestFleetQRDrawsOnlyForLocalViewerOfLANDaemon", 0],
  ["TestFleetQRRefusesRatherThanMintingAFleetNobodyAskedFor", 0],
  ["TestFleetQRRefusesToPromiseLANAccessFromLoopbackDaemon", 0],
  ["TestFleetStateDirDefaultsToTheRunningDaemonsState", 0],
  ["TestForkProviderFailureCommitsChildBeforeSelectionAndRetryIsDurable", 0.1],
  ["TestForkRetryAfterChildActorCommitAttachesExactlyOnce", 0.28],
  ["TestForkRetryAfterChildCommitDoesNotReadSourceOrRecreateLane", 0.3],
  ["TestFrontierHostsPreferPackagedRuntimeOverMutableSourceTree", 0],
  ["TestFrontierTurnAuthFailureMarksProviderNeedsLogin", 0.01],
  ["TestFrozenQueueResumeBoundaryIsInertAndActorIndependent", 0],
  ["TestFrozenWirePublicationWaitsForDurableActorCommit", 0.03],
  ["TestGenericACPCreationBoundaryComesFromNegotiatedAttachment", 0],
  ["TestGenericPlanUsageSourceDoesNotParseProviderProtocol", 0],
  ["TestGenericToolContextUsesFreshProcessEnvironmentAcrossAttachments", 0.01],
  ["TestGlobalSessionStoreCompactsLegacyMutationReceipts", 0.01],
  ["TestGlobalSessionStoreNoopReceiptIsBoundedAndUnchanged", 0.02],
  ["TestGlobalSessionStoreRejectsChatRows", 0],
  ["TestHarnessAbsentListsAreUnknownNotQuiet", 0],
  ["TestHarnessBackgroundTaskDemotesDeclaredDone", 0],
  ["TestHarnessChangedTaskStatusIsFreshEvidence", 0],
  ["TestHarnessNeverOverrulesNeedsInput", 0],
  ["TestHarnessOneShotCronParks", 0],
  ["TestHarnessQuietTurnIsDone", 0],
  ["TestHarnessRecurringCronDoesNotPark", 0],
  ["TestHarnessRepeatedTaskDoesNotReArmPark", 0],
  ["TestHarnessResolvesUnknownToDone", 0],
  ["TestHarnessWithoutHookEvidenceIsIncomplete", 0],
  ["TestHibernatedControlWriteDoesNotReviveBridgeWithoutSessionRestore", 0.15],
  ["TestHumanAuthoredTurnIsNotAdopted", 0],
  ["TestIdentityAdvertisesFleetIDsOnlyWhenItHasThem", 0],
  ["TestIdentityReportsSecureOnlyWithACertificate", 0],
  ["TestIdleSendStaysInTranscriptDuringProviderAttachment", 0],
  ["TestImageRichHistoryStartupAndPagingPreserveEveryActorRow", 0.06],
  ["TestImmediateStopPublishesCommittedTerminalBeforeReply", 0.6],
  ["TestIncompleteImageDoesNotRescanOutputOrResolveFilesPerChunk", 0],
  ["TestIncrementalAssistantImageParserMatchesWholeAnswerAcrossSplits", 0],
  ["TestInitialCatalogAuthenticationFailureKeepsClaudeLoginPolicy", 0.01],
  ["TestJobPublicCarriesBoundedAssistantImages", 0],
  ["TestLaneControlRPCRejectionIsNotTransportFailure", 0],
  ["TestLaneDiagnosticsBoundRedactAndIsolateExactPairs", 0],
  ["TestLaneDiagnosticsModelRejectionDoesNotSubmitOrReplaceThread", 0.08],
  ["TestLaneDiagnosticsRetainExactResumeFailureBeforePrompt", 0.16],
  ["TestLaneDiagnosticsRetainFailureAfterSuccessfulResume", 0.08],
  ["TestLaneDiagnosticsRetainModelFailureWithoutRawRPCData", 0],
  ["TestLateChatEnvRefreshCannotOverwriteOrPublishPastANewerTurn", 0],
  ["TestLatestCLIVersionRequiresComparableRegistryVersion", 0],
  ["TestLateToolUpdateCannotAttachToTheNextTurn", 0],
  ["TestLegacyDevinNeedsLoginRecoveryFailsClosedWhenClaimCannotPersist", 0.11],
  ["TestLegacyDevinNeedsLoginRecoveryFailureDoesNotLoopAcrossRestart", 1.07],
  ["TestLegacyPendingWakeSnapshotIsIgnored", 0.01],
  ["TestLenientSemverCompareEdges", 0],
  ["TestLifecycleIdleReapFires", 0.07],
  ["TestLifecyclePinnedNeverReapedTinyTTL", 0.19],
  ["TestLifecycleRaceReapAbortedByArrivingPrompt", 0.38],
  ["TestLifecycleRecycleAtIdleBlockedBySpawnedWork", 0.03],
  ["TestLifecycleRecycleAtNextIdle", 0.19],
  ["TestLifecycleRSSSampledForLiveChild", 0.04],
  ["TestLifecycleRunningSpawnedWorkBlocksIdleTTLHibernation", 0.02],
  ["TestLifecycleSettledSpawnedWorkAllowsIdleTTLHibernation", 0.03],
  ["TestLifecycleSilentPathlessSpawnedWorkRemainsPinnedBeyondTTL", 0.04],
  ["TestLifecycleWithoutExactResumeFailsClosedAfterHibernation", 0.37],
  ["TestListenerInAChildProcessStillClassifiesTheLane", 0.01],
  ["TestListenerThatKeepsWritingStaysWork", 0.01],
  ["TestLiveSessionProjectsNegotiatedPlanUsageCapabilities", 0],
  ["TestLoadedActorLookupDoesNotSnapshotActorState", 0],
  ["TestLocalizedWorkassToolCardCannotSelectHumanReplyLanguage", 0],
  ["TestLocalRecoveryShutdownHandlerAcceptsOnlyLoopbackPost", 0.03],
  ["TestLocalUpdateControlIsLoopbackOnlyAndCanCancel", 0],
  ["TestLocalUpdateControlRecordsActiveWorkAndExactCommit", 0.03],
  ["TestMachinesAddAndForget", 0.02],
  ["TestMachinesAddReportsFailuresAsResults", 0.01],
  ["TestMachinesListStartsWithOnlySelf", 0],
  ["TestMachinesNicknameIsPersistedInTheControllerBook", 0.02],
  ["TestManagedProviderJobRejectsLateEventsAfterTerminalCleanup", 0],
  ["TestManagerEmitSerializesDurableObserveAckAndPublication", 0.1],
  ["TestManagerLaneBackpressuresInsteadOfDroppingNormalizedEvents", 0.07],
  ["TestManagerWithoutExplicitStateDirCannotWriteRepositoryLedger", 0],
  ["TestMergedEnvDropsBlockedKeysFromInheritedAndExplicitValues", 0],
  ["TestMergeRemoteAgentChatListAdmitsOnlyTaggedExactPairs", 0],
  ["TestMetadataOnlyProjectionRetainsUncommittedForegroundRows", 0],
  ["TestMissingProviderDefinitionFailsClosedBeforeBridgeSpawn", 0],
  ["TestMockAppUpdatePayloadFromEnv", 0],
  ["TestMockBurstStreamsAtDisplayCadenceWithoutDroppingText", 2.35],
  ["TestMockClaudeProviderForwardsSpawnedWorkWithoutAgentCooperation", 0.11],
  ["TestMockClaudeProviderKeepsUnnotifiedBackgroundWorkRunningViaOutputOwner", 0.59],
  ["TestMockInitializeSessionPromptCancelErrorAndReuse", 1.29],
  ["TestMockLostTerminalDoesNotRecycleProviderBridge", 0.28],
  ["TestMockLostTerminalIsOwnedByHarnessUntilExplicitCancel", 0.28],
  ["TestMockNativeSessionForeignLiveCollisionFailsClosed", 0.09],
  ["TestMockNativeSessionLoadAttachesTheExactThreadWithoutPublishingReplay", 0.2],
  ["TestMockNativeSessionNeverResumesAfterConversationIdentityChanges", 0.14],
  ["TestMockNativeSessionResumeAfterHibernationDoesNotCollide", 0.23],
  ["TestMockNativeSessionResumesExactThreadAcrossManagerRestart", 0.18],
  ["TestMockNativeSessionUnseenWorkassHistoryDoesNotGovernExactResume", 0.25],
  ["TestMockNaturalAssistantImageCompletesAsDurableJobMedia", 0.14],
  ["TestMockPermissionMarkerRoundTrip", 0.15],
  ["TestMockPromptSilenceDoesNotCompleteAnAuthoritativelyActiveTurn", 0.27],
  ["TestMockProviderTypedPhasesAreExplicitAndPhaseLessTurnsStayPlain", 0.21],
  ["TestMockSteerMidSlowTurnReflectedInOutput", 1.3],
  ["TestMoveWorkspaceReceiptReplayFinishesCrashWindowWithoutNewEpoch", 0.24],
  ["TestMoveWorkspaceReceiptRetryDoesNotCloseProviderHostAgain", 0.29],
  ["TestMultipleWorkspaceEpochsFailClosedInsteadOfSelectingOrCreating", 0.02],
  ["TestNativeAgentStopIsReadOnlyEvenWithProcessMetadata", 0],
  ["TestNativeChatPromptPreservesHistoryWithoutBoilerplate", 0],
  ["TestNativeCodexBackgroundChildOutlivesParentAndRetainsReceipt", 0.17],
  ["TestNativeCodexBackgroundLifecycleIsOwnedByChatActor", 0.38],
  ["TestNativeCodexSubagentsReachDaemonToolEvents", 0.13],
  ["TestNativeInstructionConfigMergeRejectsMalformedAndDeduplicates", 0],
  ["TestNativeInstructionsBindStableContextWithoutChangingUserConfig", 0.01],
  ["TestNativeLaneOwnershipSurvivesDisposableTabChange", 0.03],
  ["TestNativeSessionBindingRequiresExactConversationOwner", 0.02],
  ["TestNativeSessionControlsWriteOnlyChangesAndRetryFailedWrite", 0.02],
  ["TestNativeSessionLedgerDeleteChatRemovesEveryProviderBinding", 0.03],
  ["TestNativeSessionLedgerDropsLegacyTurnGateOnLoad", 0.01],
  ["TestNativeSessionLedgerNeverPrunesBindingsFromRendererMirror", 0.03],
  ["TestNativeSessionLedgerPersistsExactSessionOnly", 0.02],
  ["TestNeedsLoginBlocksStaleSessionUntilExplicitReenable", 0.02],
  ["TestNeedsLoginRejectsStaleSessionAndChatProviderBindings", 0],
  ["TestNegotiatedACPSteeringSurvivesProviderLaneSelection", 0.16],
  ["TestNestedSubagentOwnershipIsImmediateAndCascadeIsScoped", 0],
  ["TestNewSessionOpensWhenStoredStartupModelIsUnappliable", 0.03],
  ["TestNextTurnListsAndWaitsOnAdoptedSubagent", 3.19],
  ["TestNormalizeCatalogModelsCollapsesVocabularyVariants", 0],
  ["TestNormalizeCatalogModelsKeepsBracketVariantOutsideEffortVocabulary", 0],
  ["TestNormalizeCatalogModelsKeepsSingleEffortFamilyUncollapsed", 0],
  ["TestNormalizeCatalogModelsSortsEffortsInCanonicalOrder", 0],
  ["TestNormalizeCatalogModelsSplitsMixedEffortAndContextVariants", 0],
  ["TestNormalizeClaudeCatalogAddsVersionsAndOrdersByPower", 0],
  ["TestNormalizedPlanUsageCaptureClearsExplicitResetSnapshot", 0],
  ["TestNormalizedPlanUsageCaptureMergesTypedSnapshots", 0],
  ["TestNormalizedPlanUsageCaptureStoresRawWithoutFabricatingEntries", 0],
  ["TestNormalizeModelScoresClampsAndDropsUnknownData", 0],
  ["TestNormalizeModelScoresPreservesTypedSettingsReload", 0],
  ["TestNormalizeSpawnedWorkRoleAcceptsOnlyCanonicalRoles", 0],
  ["TestNoWorkassMCPDescriptorsForAnySession", 0],
  ["TestObsoleteSpawnedWorkConversionSurfaceIsPhysicallyAbsent", 0],
  ["TestOfficialNativeHostsExposeExactResumeOnly", 0],
  ["TestOlderBridgeCannotReplaceLaterAuthoritativeProbe", 0],
  ["TestOMLXAwareLaunchInjectsProviderOwnedKeyEphemerally", 0],
  ["TestOMPBridgeSteersNativeSDKWithoutQueueOrInterrupt", 0.1],
  ["TestOMPHostUsesInstalledCommandAndSharedNode", 0],
  ["TestOMPInstalledHostContract", 1.07],
  ["TestOMPKnownPathsCoverOfficialWindowsInstallers", 0],
  ["TestOMPNativeHostContract", 0.47],
  ["TestOMPNativeInstructionsAndPermissionMapping", 0],
  ["TestOMPRegistrationUsesNativeSDKEntryPoint", 0],
  ["TestOMPRejectsMissingInstalledExecutable", 0],
  ["TestOpenMachineBookNeedsAnIdentity", 0],
  ["TestOrdinaryMCPMentionIsNotMisclassifiedAsToolCard", 0],
  ["TestOrdinaryPromptCarriesStableWorkassOperationID", 0.02],
  ["TestOversizedImageFailsBeforeDurableJobAdmission", 0.02],
  ["TestParentDenyReleasesAParkedSubagentPermission", 0.01],
  ["TestParentMayGrantOnlyFromItsOwnFullAccessMode", 0],
  ["TestParseTasklistRSSKB", 0],
  ["TestParseTasklistRSSKBNoMatch", 0],
  ["TestPendingSubagentPermissionIgnoresOtherSessions", 0.05],
  ["TestPermissionAttentionDoesNotDeadlockWithALiveParentJob", 0],
  ["TestPermissionAttentionNamesWhoCanGrantIt", 0],
  ["TestPermissionIntentInheritanceTranslatesAcrossProviders", 0],
  ["TestPermissionIntentModesUsesProviderNativeIds", 0],
  ["TestPermissionOptionForDecisionReadsAllowAndReject", 0],
  ["TestPermissionQuestionIsBoundedAndRedacted", 0],
  ["TestPermissionQuestionRejectsUnanswerableInput", 0],
  ["TestPermissionRequestForwardsTheAgentsQuestion", 0],
  ["TestPermissionRequestWithoutQuestionStaysAPlainPermission", 0],
  ["TestPermissionResolutionEmitsTerminalReceiptForDecisionAndCancellation", 0],
  ["TestPermissionWaitRemainsOwnedByHarness", 0.17],
  ["TestPermissionWithoutAConfiguredDeadlineArmsNoTimer", 0],
  ["TestPhaseCAgentWaitBindsOperationIDBeforeTransientManager", 0.04],
  ["TestPhaseCChatIDExecutorMutationInventoryIsExplicit", 0.04],
  ["TestPhaseCChatIDStubIngressIsExplicit", 0.04],
  ["TestPhaseCChatIngressManifestIsComplete", 0.04],
  ["TestPhaseCManagerBoundaryIsExplicit", 0.04],
  ["TestPhaseCManagerLanePreservesBurstAndTerminalUnderBoundedBackpressure", 0.06],
  ["TestPhaseCManagerPublicationWaitsForDurableActorState", 0.11],
  ["TestPhaseCObsoleteExecutorConversionAPIIsAbsent", 0.04],
  ["TestPhaseCObsoleteTranscriptStoreIsAbsent", 0.05],
  ["TestPhaseCRecoveryExecutorCallbacksCarryOperationIdentity", 0.04],
  ["TestPhaseCSessionStoreSurfaceIsExplicit", 0.04],
  ["TestPhaseCStatelessMCPMutationBoundaryIsExplicit", 0.04],
  ["TestPhaseCStatelessMCPOperationManifest", 0],
  ["TestPiBridgeSteersNativeSDKWithoutQueueOrInterrupt", 0.07],
  ["TestPiDiscoveryUsesOfficialSDKHost", 0.21],
  ["TestPiHostUsesInstalledCommandAndSharedNode", 0],
  ["TestPiNativeHostContract", 0.39],
  ["TestPiNativeInstructionsAndPermissionMapping", 0],
  ["TestPiNativeSDKProviderContext", 1.72],
  ["TestPiNativeWindowsTrustEnvironment", 0],
  ["TestPiRejectsMissingInstalledExecutable", 0],
  ["TestPlanUsageCarrierDispatchUsesRegisteredStrategy", 0],
  ["TestPlanUsageClaudeStructuredRefreshCapturesFiveHourAndWeeklyWindows", 0.02],
  ["TestPlanUsageCodexStructuredRefreshCapturesFiveHourAndWeeklyWindows", 0.02],
  ["TestPlanUsagePeriodicTickRefreshesOneLiveSessionPerProviderWithoutPrompt", 0.04],
  ["TestPlanUsageRawIsBoundedAndRedacted", 0],
  ["TestPlanUsageRefreshDefaultsToFiveMinutes", 0],
  ["TestPlanUsageRefreshesOnSessionAttachAndTerminalTurn", 0.03],
  ["TestPlanUsageRefreshIsCancelledByReset", 0.02],
  ["TestPlanUsageReplayToLateClient", 0],
  ["TestPlanUsageTerminalRefreshQueuesBehindSlowAttachRefresh", 0.17],
  ["TestPrefilterFoldsASCIICaseOnly", 0],
  ["TestPrefilterNeverSkipsARedactableString", 0.02],
  ["TestPresentationWithoutDraftNeverMutatesLegacyDraft", 0.07],
  ["TestPreserveUnknownModelEffortsKeepsOnlyUninspectedCapabilities", 0],
  ["TestPreTurnCheckpointCapturesWorktreeOnce", 0.21],
  ["TestProductionGlobalSessionStoreHasNoChatSemanticAuthority", 0.01],
  ["TestProductionMockProviderCannotInjectSpawnedWork", 0],
  ["TestProductionMockProviderCannotRegisterExternalWork", 0],
  ["TestProductionRuntimeHidesAndRejectsFixtureModels", 0],
  ["TestProjectActorChatRendersAmbiguousAdmissionAsBlockedInsteadOfRunning", 0],
  ["TestProjectIconPrefersConfiguredPathAndReturnsNoFilesystemPath", 0.01],
  ["TestProjectIconReadsDeclaredRootRelativeSVG", 0],
  ["TestProjectIconRejectsUnsafeCandidatesAndFallsThrough", 0],
  ["TestProjectIconWireReadDegradesToMissing", 0],
  ["TestProjectLedgerMessageUsesOnlyRichActorState", 0],
  ["TestProjectSessionMetadataOptimizationPreservesFallbackAndActivity", 0],
  ["TestProjectSessionPreservesManualChatOrder", 0.16],
  ["TestPromptBlocksKeepsTheLeadingSlashAheadOfTheImageNotice", 0],
  ["TestProviderAdapterDefaultsAllowSingleFacetOverride", 0],
  ["TestProviderAdmissionReceiptIsOwnedByChatNotDisposableTab", 0],
  ["TestProviderAuthenticationRuntimeUsesDefinitionWithoutRegistrationFallback", 0],
  ["TestProviderCatalogRefreshDoesNotProbeDisabledOrNeedsLoginProvider", 0],
  ["TestProviderChatAgentReadBoundsEventHeavyTranscriptForMCPRelay", 0.13],
  ["TestProviderChatAgentReadProjectsActorBackgroundState", 0.31],
  ["TestProviderChatAgentReadRejectsWrongPairBeforeManagerCapability", 0.06],
  ["TestProviderChatAgentWaitChangedIntentWinsWhenTargetIsMissing", 0.29],
  ["TestProviderChatAgentWaitFencesStalePairBeforeOwnerManager", 0.33],
  ["TestProviderChatAgentWaitManyUsesTerminalActorRows", 0.35],
  ["TestProviderChatAgentWaitObservationRaceReservesOneReceipt", 0.3],
  ["TestProviderChatAgentWaitUsesDurableObservationReceipt", 0.31],
  ["TestProviderChatCloseSessionChangedActorTargetFailsClosed", 0.25],
  ["TestProviderChatCloseSessionDetachesCurrentAttachmentPreservingThread", 0.29],
  ["TestProviderChatCloseSessionRejectsStaleConnectionID", 0],
  ["TestProviderChatCloseSessionRetryCannotCloseExactResumedAttachment", 0.45],
  ["TestProviderChatRuntimeCheckpointExecutorCarriesOperationToReceipt", 0.13],
  ["TestProviderChatRuntimeResumesExactLaneAcrossActorAndTabRestart", 0.59],
  ["TestProviderChatRuntimeSwitchesAndReturnsThroughVerifiedContextImport", 0.8],
  ["TestProviderChatStartChangedImageRetryDoesNotPersistSidecar", 0.32],
  ["TestProviderChatSteerDuplicateAndChangedRetryAreDurableReadbackOnly", 0.42],
  ["TestProviderChatSteerRejectedInputDoesNotPersistAttachmentSidecar", 0.19],
  ["TestProviderChatSteerRejectsAfterForegroundEndWithoutTakingOwnership", 0.2],
  ["TestProviderChatSteerRejectsStaleDurableAttachmentBeforeManagerOrSidecars", 0.23],
  ["TestProviderChatSteerTerminalWinnerNeverFallsBackToFIFO", 0.29],
  ["TestProviderCLIExecutableRefreshesValidCacheFromPATH", 1.08],
  ["TestProviderCLIExecutableUsesExplicitPathVariable", 0.38],
  ["TestProviderConfigFileDefaultsAndConfiguredRegistry", 0],
  ["TestProviderDetectionAllowsFullInitializeAndSessionBudgets", 6.14],
  ["TestProviderDetectionCollapsesEffortVariantCatalog", 0.01],
  ["TestProviderDetectionDefaultRetryCadenceMatchesPortContract", 0],
  ["TestProviderHeadAdvanceRequiresAttestedMonotonicLineage", 0.02],
  ["TestProviderLaneArmsDurableCommitsBeforeLaneOpened", 0.2],
  ["TestProviderLaneCommandCatalogUpdateIsActorDurableBeforePublication", 0.03],
  ["TestProviderLaneManagedChatEnvInitializationDoesNotBlockSession", 0],
  ["TestProviderLaneMovedToAnotherMachineIsRejectedAtLoad", 0.01],
  ["TestProviderLaneRejectsUnknownFrozenSemanticEvents", 0.17],
  ["TestProviderLaneSelectionIsReadOnlyAndReturnsExactStoredBinding", 0.03],
  ["TestProviderLaneSelectionIsReadOnlyUntilAtomicReceiptCommit", 0.25],
  ["TestProviderLaneSelectionRetryCreatesAfterOldZeroThreadFailure", 0.41],
  ["TestProviderLaneStoreRejectsUnsupportedVersionWithoutWriting", 0],
  ["TestProviderNativeCompactionBypassesWorkassFallback", 0.26],
  ["TestProviderNativeThreadIDsAreScopedByProviderRealm", 0.02],
  ["TestProviderNotificationAdaptersIsolateVendorFrames", 0],
  ["TestProviderOwnsContextCompaction", 0],
  ["TestProviderPlanUsageStrategiesNormalizeOnlyTheirRegisteredProtocol", 0],
  ["TestProviderPlanUsageStrategiesNormalizeQuotaAndResetClearing", 0],
  ["TestProviderPlanUsageStrategiesNormalizeStructuredWindows", 0],
  ["TestProviderPrivateTokensStayAtRegisteredBoundaries", 0.24],
  ["TestProviderRegistrationsPublishVerifiedAssistantBrands", 0],
  ["TestProviderRegistryCatalogToggleFailureAndConcurrentIsolation", 1.37],
  ["TestProvidersListHidesOnlyUnconfiguredCustomPlaceholder", 0],
  ["TestProviderSpawnedWorkAdapterDecodesTypedSignals", 0],
  ["TestProviderSpawnedWorkAdaptersRejectUnregisteredAndUnsafeData", 0],
  ["TestProviderTypedMessagePhaseBoundarySurvivesStdoutCoalescing", 0],
  ["TestProviderUpdateAvailabilityUsesCardWithoutNotify", 0],
  ["TestProviderUpdateCheckFakeRegistry", 1.82],
  ["TestProviderUpdateCheckRegistryFailuresOmitEntries", 0.03],
  ["TestProviderUpdateCheckSkipsExplicitlyDisabledProvider", 0],
  ["TestProviderUpdateFailureDoesNotLeakToDifferentTarget", 0],
  ["TestProviderUpdateInvokeFailureKeepsCardWithRedactedTail", 2.26],
  ["TestProviderUpdateInvokeProgressNoProcRegistryAndReplay", 2.81],
  ["TestProviderUpdateInvokeRejectsDoubleUnknownAndNoPending", 3.81],
  ["TestProviderUpdatePostRecheckAllFailKeepsEntryWithRecheckError", 1.31],
  ["TestProviderUpdatePostRecheckRetriesUntilVersionLands", 1.35],
  ["TestProviderUpdateRunsResolvedProviderExecutable", 2.15],
  ["TestProviderUpdateRunTerminalNotCounted", 0],
  ["TestProviderUpdateScheduledChecksAreSingleFlight", 0],
  ["TestProviderUpdateSchedulerDefaultCadence", 0],
  ["TestProviderUpdateSchedulerRepeatsWithoutDaemonRestart", 0.01],
  ["TestProviderUpdateSchedulerRetriesPartialRegistryFailure", 0.02],
  ["TestProviderUpdateSchedulerRetriesRegistryFailure", 0.02],
  ["TestProviderUpdatesPayloadSkipsIncomparableInstalledVersion", 0],
  ["TestProviderUpdatesRequireInstalledCLIAndPublishCardRemoval", 0],
  ["TestProviderUpdateTerminalReceiptDoesNotWaitForRegistryRefresh", 1.18],
  ["TestProviderUpdateZeroExitWithoutVersionAdvanceFailsVerification", 2.11],
  ["TestProviderVersionChangeReprobesAndPublishesCatalog", 0.05],
  ["TestProviderWithoutNativeCompactionNeverReplacesOrReseedsLane", 0.23],
  ["TestPruneSubagentsNeverEvictsRunningAdoptedRun", 0],
  ["TestPublicationOrderingSurvivesDifferentAttachmentsOfSameChat", 0.05],
  ["TestQuestionWaitsForTheUserWhileAPermissionStillExpires", 0.62],
  ["TestQuietListenerBecomesAServiceAndStopsReportingTheChatBusy", 0],
  ["TestQuietProcessWithoutAListenerStaysWork", 0.01],
  ["TestQwenStandaloneUpdateUsesBundledUpdaterAtCompatibleRelease", 2.79],
  ["TestRawMCPDockerCommandLineMatchesOnlyBlockedDirectImages", 0],
  ["TestRealCopiedStateStartup", 0],
  ["TestRealNativeFrontierCatalogs", 0],
  ["TestRealOMLXQwenTurn", 0],
  ["TestRealOMPProtocolCatalog", 0],
  ["TestRealOMPTurn", 0],
  ["TestRealProviderUpdateCheck", 0],
  ["TestRealProviderUpdateInvokeQwen", 0],
  ["TestRecommendationTreatsHigherCostAsLessDesirable", 0],
  ["TestReconcileClaudeLiveCatalogPreservesProbedContextVariant", 0],
  ["TestRefreshPlanUsageSessionReusesLiveChatWithoutPrompt", 0.03],
  ["TestRefreshProviderPlanUsageColdStartIsEphemeralAndPromptFree", 0.01],
  ["TestRefreshProviderPlanUsageReusesAnyLiveProviderSessionWithoutPrompt", 0.02],
  ["TestRegisteredACPProvidersKeepTheirActualSteeringPath", 0],
  ["TestRegisteredExternalServiceLaneIsClassifiedFromItsListeningChild", 0.02],
  ["TestRegisteredNotificationAdapterDecodesToolParentID", 0],
  ["TestRejectedClaudeSteerLeavesForegroundAndSpawnedWorkRunning", 0.05],
  ["TestRejectedSteerDoesNotEndParentOrPrematurelyAdoptRunningSubagents", 0.07],
  ["TestRemoteAgentChatTargetRequiresOneExactMachine", 0],
  ["TestRendererAgentRouterFailsImmediatelyWithoutLocalRenderer", 0],
  ["TestRendererAgentRouterRoundTripKeepsOwnerCapabilityOutOfRenderer", 0],
  ["TestRendererChatCreationIsDurableIdempotentAndIndependentFromProviderAttachment", 0.09],
  ["TestRendererPresentationReceiptSurvivesLostReplyAndRevisionHydration", 0.09],
  ["TestRepeatedSessionImagesPreservePayloadAndVerifyEachNewRead", 0.02],
  ["TestReplaceStagedQueueStaleRevisionDoesNotPersistAttachmentSidecar", 0.21],
  ["TestResetMarksInFlightTurnInterrupted", 0.07],
  ["TestResolveAssistantMarkdownImagesImportsNaturalWorkspaceLinks", 0],
  ["TestResolveAssistantMarkdownImagesRejectsNonWorkspaceAndUnsafeMedia", 0],
  ["TestResolveClaudeSyntheticDefaultAliasRequiresUniqueMetadataMatch", 0],
  ["TestResolveFrontierNativeLaunchHonorsExplicitOfficialCLIOverrides", 0],
  ["TestResolveFrontierNativeLaunchUsesOfficialCLIsAndIgnoresZedAdapters", 0],
  ["TestResolveMocksDirPrecedenceAndExecutableDiscovery", 0.01],
  ["TestResolveVisualizationPathAllowsOnlyExactWorkspaceSibling", 0.05],
  ["TestResolveWorkassAgentLaunchOrder", 0],
  ["TestResolveWorkassToolsCommandFailsClosedAndAllowsOnlyExplicitDevOverride", 0.01],
  ["TestResolveWorkassToolsCommandUsesDaemonWithoutSibling", 0.01],
  ["TestRuntimeBackgroundOwnerRequiresExactOrigin", 0],
  ["TestRuntimeControlsCommitToActorBeforeProviderAndApplyOnlyAtTurnBoundary", 0.45],
  ["TestRuntimeDiagnosticsAdmissionFailureFlushesOnReset", 0.06],
  ["TestRuntimeDiagnosticsBoundedContentFreeAndImmutable", 0],
  ["TestRuntimeDiagnosticsCheckpointSurvivesRestartWithoutInventedCompletion", 0.01],
  ["TestRuntimeDiagnosticsCoalescedFailureGetsTrailingCheckpoint", 1.01],
  ["TestRuntimeDiagnosticsConcurrentCheckpointAndReaders", 0.01],
  ["TestRuntimeDiagnosticsCrashStagingIsBounded", 0.01],
  ["TestRuntimeDiagnosticsExactTurnWithoutActivityOrConsumption", 0],
  ["TestRuntimeDiagnosticsRejectUnsafeStoreAndExposeWriteFailure", 0],
  ["TestRuntimeDiagnosticsSeparatesHostTransportAndProviderError", 0],
  ["TestRuntimeDiagnosticsWriterDoesNotBlockProviderOrReads", 0.01],
  ["TestSaveProviderConfigsConcurrentWritersUseDistinctTemps", 0.01],
  ["TestServeDaemonHTTPStopsAcceptingAndRunsCleanup", 0],
  ["TestServiceTierRefusesUnadvertisedControls", 0.06],
  ["TestSessionAttachCannotCrossUnprobedCLIVersionInvalidation", 0],
  ["TestSessionAttachFromOlderSameVersionProcessRetainsCatalogRevisionFence", 0],
  ["TestSessionAttachPublishesChangedModelOptions", 0],
  ["TestSessionAttachRefreshesCatalogRevisionForLiveBridge", 0],
  ["TestSessionDeliveryCapabilitiesUseTypedCamelCaseWireShape", 0],
  ["TestSessionImageNameMemoIsByteBoundedAndNotPayloadKeyed", 0.04],
  ["TestSessionProjectionCarriesHistoryOnlyForActiveOrRunningChats", 0],
  ["TestSessionRefreshCoordinatorCoalescesTargetsAtHighestGeneration", 0.06],
  ["TestSessionRefreshCoordinatorDeadlineDoesNotReset", 0.08],
  ["TestSessionRefreshCoordinatorFocusIsOneShotBeforeGenericRefresh", 0],
  ["TestSessionRefreshCoordinatorImmediateAndMergedEventsAreRendererEquivalent", 0.02],
  ["TestSessionRefreshCoordinatorMeasuredBurst", 0.06],
  ["TestSessionRefreshCoordinatorMutationDuringFlushUsesNextDeadline", 0.03],
  ["TestSessionStoreRemovedRuntimeSurfaceIsPhysicallyAbsent", 0],
  ["TestSetModelPassesEffortSuffixedIDUnchanged", 0.07],
  ["TestSettledBackgroundWorkNeverExposesOrSchedulesWakeState", 0],
  ["TestSilentAgentTurnSurfacesEmptyCompletionNotice", 0.19],
  ["TestSpareAdoptionRebindsInjectedAgentOwnerToRealChat", 0.02],
  ["TestSpareWarmingDemotesDevinAuthenticationFailureAndStops", 0.11],
  ["TestSpawnableProviderIDsListsOnlyEnabledOnesSorted", 0],
  ["TestSpawnedSubagentQuestionIsHandedBackWhileItsPermissionsStillAsk", 0],
  ["TestSpawnedWorkBatchProbeFindsOpenOutputOwner", 0.14],
  ["TestSpawnedWorkCommitCompactsOnlyAfterActorAcceptsSnapshot", 0.01],
  ["TestSpawnedWorkCommitDoesNotPersistBeforeActorAcceptance", 0],
  ["TestSpawnedWorkFallbackNormalizesSentencePunctuationAndMergesStructuredRecord", 0],
  ["TestSpawnedWorkListCarriesTheObligation", 0.37],
  ["TestSpawnedWorkPassivelyTracksBackgroundBashAndWritesReceipt", 0.09],
  ["TestSpawnedWorkRejectsUntrustedOutputPaths", 0],
  ["TestSpawnedWorkRestoredPathlessRecordBecomesOrphanedButLiveSilentRecordStaysRunning", 0.01],
  ["TestSpawnedWorkSnapshotReloadDropsNoncanonicalCacheRow", 0.01],
  ["TestSpawnedWorkStructuredAgentWorkflowAndSnapshotLifecycle", 0],
  ["TestSplitEffortSuffix", 0],
  ["TestStaleSelectionRejectsBeforeEnvironmentRestore", 0.1],
  ["TestStandardACPLaunchDoesNotInjectWorkassTrust", 0],
  ["TestStandardACPLaunchPreservesUserTrustWithoutReadingOrWritingCertificates", 0],
  ["TestStartJobStaleSessionIDCannotDriveAnotherChat", 0.14],
  ["TestStartupDetectionDoesNotRetryDevinNeedsLogin", 0.43],
  ["TestStartupDetectionRecoversLegacyDevinNeedsLoginOnceUnderSanitizedLaunch", 0.67],
  ["TestStartupDetectionSkipsPersistedNeedsLoginUntilExplicitProbeSucceeds", 0.48],
  ["TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession", 1.08],
  ["TestStartupDetectProvidersRetriesOnlyStatusErrors", 0.88],
  ["TestStartupTimingMeasuresMockWireAndExcludesContent", 0.21],
  ["TestStartupTimingRetainsFirstObservation", 0],
  ["TestStateDigestIsBodyFreeAndUnder64KiBForTwoHundredChats", 0.01],
  ["TestStatelessMCPMutationsRequireCallerStableOperationID", 0.36],
  ["TestStatelessMCPRoutesTaggedRemoteReadWithoutExposingOwner", 0.42],
  ["TestStatelessMCPRoutesUpdaterStatusAndAuthorizedApplyWithoutExposingOwner", 0.39],
  ["TestStatelessMCPSpawnsAndWaitsForTrackedSubagent", 0.89],
  ["TestSteerAttachmentInputIdentityMatchesPersistedAttachment", 0.03],
  ["TestSteerRejectsInternalMaintenancePrompt", 0.02],
  ["TestSteerSucceedsOnlyAfterBasePromptPhysicalWrite", 0.14],
  ["TestSteerUnsupportedRejectsWithoutQueueingOrInterrupting", 0.18],
  ["TestSteerWaitsForCommittedBasePromptDispatchBoundary", 0.05],
  ["TestStopCancelsBlockedTurnControlPreparation", 0.05],
  ["TestStopDoesNotWaitForUnrelatedProviderAttachment", 0.37],
  ["TestStopSpawnedWorkKillsARealProcessThatIgnoresSIGTERM", 2.02],
  ["TestStopSpawnedWorkNeverSignalsTheDaemonOrInit", 0.02],
  ["TestStopSpawnedWorkRejectsUnknownRows", 0],
  ["TestStopSpawnedWorkSettlesALaneWithNoLiveProcess", 0.02],
  ["TestStopSpawnedWorkSignalsEveryProcessHoldingTheLaneOpen", 0.02],
  ["TestStreamStatsDoNotMixIndependentBridgeGaps", 0.03],
  ["TestStreamStatsIgnoreIdleGapsBetweenTurns", 0],
  ["TestStreamStatsSeparateAgentPaceFromDaemonWork", 0],
  ["TestSubagentAPIDeniesMissingOrStaleOwner", 0],
  ["TestSubagentBriefOmitsTheBrowserItDoesNotHave", 0],
  ["TestSubagentCancelClearsPermissionAndValidOwnerIsolation", 0.28],
  ["TestSubagentEventOnlyWaitCancellationDoesNotCancelChild", 0.16],
  ["TestSubagentExitPlanModeAnswersItselfWithoutGranting", 0],
  ["TestSubagentHookIsNotASessionTurn", 0],
  ["TestSubagentLatchedPermissionAttentionSurfacesToAdoptingTurn", 1.84],
  ["TestSubagentModelLabelBuildsTurnosChip", 0],
  ["TestSubagentPermissionAttentionStaysUnreadAfterFastResolution", 0],
  ["TestSubagentPermissionInheritanceUsesEffectiveLiveModeAndNeverDowngrades", 0.07],
  ["TestSubagentQuestionGoesBackToTheParentInsteadOfTheUser", 0.05],
  ["TestSubagentSurvivesCancelledParentSettlesAndWritesReceipt", 2.68],
  ["TestSubagentTurnlessOwnerListsWaitsAndSpawnsBornAdoptedWithOptionalVisibleHint", 0.48],
  ["TestSubagentWaitDoesNotReportTerminalBeforeReceiptCommit", 0],
  ["TestT10ExternalSnapshotCapPreservesAllRunningRecords", 0.02],
  ["TestT11ExternalWorkPublicPayloadsRedactSecretShapedOutputPath", 0.01],
  ["TestT1ColdBridgePreservesUndiscoveredEffortSelection", 0.04],
  ["TestT1cUndiscoveredBaseRowStillRoutesEffortComposite", 0.02],
  ["TestT1ExternalWorkRegisterDesignatesOutputAndPersistsSnapshot", 0.03],
  ["TestT2AuthoritativeEffortDowngradePersistsAndLogs", 0.07],
  ["TestT2RunningExternalWorkDoesNotPinIdleTTLHibernationButBashStillDoes", 0.03],
  ["TestT3AdapterSideModelChangeCapturesStoredControls", 0.07],
  ["TestT3ExternalDoneFileSettlesAndWritesRedactedReceiptTail", 0.32],
  ["TestT4ExternalDeadPIDSettlesAfterMissingGrace", 0.13],
  ["TestT5LiteralBracketedModelRoundTripsAfterResumeDiscovery", 0.03],
  ["TestT6NewSessionConvergesNativeBindingModelWithoutRendererReapply", 0.03],
  ["TestT7AgentControlExternalSettleIsIdempotentAndOwnerValidated", 0.66],
  ["TestT9ExternalWorkPathValidation", 0],
  ["TestTerminalFlushWaitsForInFlightStreamPublication", 0.02],
  ["TestTerminalPublicationDoesNotWaitForAnotherChat", 0],
  ["TestToolAPIRefusesPlaintextAndBrowserOrigin", 0.41],
  ["TestToolContextDoesNotAdvertiseForUnconfiguredSessions", 0],
  ["TestToolContextIsPrivateStableAndBoundToCurrentSession", 0],
  ["TestToolListenerReadinessPrecedesProviderStartupRelease", 0.01],
  ["TestToolResultImagesAreBounded", 0],
  ["TestToolsAPIRejectsMCPAndWrongOwner", 0.43],
  ["TestToolsCLIBrowserImageAndArgumentsFile", 0.41],
  ["TestToolsCLICatalogCallAndMutationReceiptsWithoutMCP", 0.49],
  ["TestToolsCLICrossProviderDelegationAndCrossChatMessaging", 1.22],
  ["TestToolsCLIEnvironmentDiscoveryAndExplicitOverride", 0],
  ["TestTrackedSubagentAppearsAndUpdatesInOwningSpawnedWorkFeed", 0.06],
  ["TestTrackedSubagentParentEngineExitDoesNotOrphan", 0],
  ["TestTrackedSubagentReconcileSilenceDoesNotOrphan", 0],
  ["TestTrackedSubagentSettleCarriesModelAndResultOnTheWire", 0.03],
  ["TestTrackedSubagentSnapshotRestartsAsOrphanedInsteadOfPhantomRunning", 0.02],
  ["TestTrackedSubagentTerminalStatusesSettleWithoutSyntheticWake", 0.29],
  ["TestTurnControlRestoreRejectionFallsBackToPrompt", 0.02],
  ["TestTurnDiagnosticsConcurrentReadsDoNotRaceStreaming", 0],
  ["TestTurnDiagnosticsLiveStopAndCompletedRetention", 0],
  ["TestTurnDiagnosticsMockCancellationRecordsWireBoundaries", 1.05],
  ["TestTurnReappliesPersistedModelAndPermissionMode", 0.09],
  ["TestTurnRoutesCodexCrossModelRestoreThroughSeparateEffortAxis", 0.08],
  ["TestTurnTranslatesLegacyPermissionAndRoutesCodexEffortAxis", 0.09],
  ["TestUnconfiguredBrowserIsNotAdvertisedPerTurn", 0],
  ["TestUnifiedCoordinatorRunsExistingACPManagerThroughTypedEvents", 0.04],
  ["TestUnifiedLaneCreateNeverRetriesAnAmbiguousProviderCreate", 0.01],
  ["TestUnifiedLaneFactoryRejectsReplacementThreadBeforeProviderCall", 0.02],
  ["TestUnownedClaudeProviderSessionCannotMutateNativeLineage", 0.02],
  ["TestUnpatchedCodexAdapterRejectsWithoutQueueingOrInterrupting", 0.04],
  ["TestUnregisterCrashedProviderLaneFencesOperationWithoutRebinding", 0],
  ["TestUnsupportedSubagentSteerKeepsOneDurableFollowupWithoutInterrupting", 0.06],
  ["TestUpdateDrainNeverBlockedByInFlightAdmission", 0],
  ["TestUpdateDrainRecordsActiveWorkWithoutBlocking", 0],
  ["TestUpdaterMCPAuthorityRuleIsAdjacentToEveryConfiguredTurn", 0],
  ["TestUpdaterMCPIsNotAdvertisedWithoutConfiguredServer", 0],
  ["TestUserCancelIsNotAnOutcome", 0],
  ["TestUserExitPlanModeIsStillTheUsersToAnswer", 0.08],
  ["TestVersionParsingRealOutputFixtures", 0],
  ["TestVisualizeHostCapturesAllowedExecutorHTML", 0.15],
  ["TestVisualizeHostCompletedRetryAfterRuntimeRestart", 0.15],
  ["TestVisualizeHostDoesNotDuplicateContentAddressedRegistration", 0.09],
  ["TestVisualizeHostFailsClosedForAmbiguousDispatchedMutation", 0.09],
  ["TestVisualizeHostFencesStaleAndDeletedActorBeforeCapture", 0.11],
  ["TestVisualizeHostLostReplyReadsActorReceiptWithoutSource", 0.15],
  ["TestVisualizeHostRecoversCrashAfterRegistryEffectWithoutDuplicate", 0.18],
  ["TestVisualizeHostRejectsChangedRequestForStableOperation", 0.05],
  ["TestVisualizeHostReturnsFailedActorTerminalState", 0.06],
  ["TestVisualizeHostSamePathChangedContentGetsNewFallbackOperation", 0.1],
  ["TestWindowsCommandScriptInvocationUsesHiddenShellBoundary", 0],
  ["TestWindowsManagedProcessBoundaryOwnsEveryPortableCommand", 0.01],
  ["TestWireBusyStartQueuesCapabilityAwareFollowUpWithoutFailedTranscript", 0.54],
  ["TestWireClientReadyAndSessionSaveDoNotCreatePlanUsageSessionOrRaceRealAttach", 0.32],
  ["TestWireClientReadyReplaysSessionRefresh", 0.01],
  ["TestWireCodexEarnedRateLimitResetConsume", 0.37],
  ["TestWireCreateDirCreatesOneExactChild", 0.01],
  ["TestWireCreateDirRejectsTraversalDuplicatesAndNonDirectoryParents", 0.01],
  ["TestWireDaemonQueueDrainsWithoutControllerAndReplaysPermissionOnAttach", 0.52],
  ["TestWireE2EAppChatAssertsJobEventChannel", 1.37],
  ["TestWireFakeACPHelper", 0],
  ["TestWireFreshProviderGetsHistorySeedAndEstablishedLaneUsesSafeImport", 0.52],
  ["TestWireJobStartReplyGateBlocksProviderAndProjectsFailureAfterReceipt", 0.23],
  ["TestWireListDirBrowsesDaemonFilesystem", 0.02],
  ["TestWireLostTerminalWaitsForHarnessOrExplicitCancel", 0.35],
  ["TestWireMockBurstReachesClientAtDisplayCadence", 1],
  ["TestWirePlanUsageReplayToLateClient", 0.35],
  ["TestWireProviderCatalogConnectBeforeDetectionGetsSingleBroadcast", 0.62],
  ["TestWireProviderCatalogReplayToLateClient", 0.38],
  ["TestWireProvidersDetectInvokeEmitsAndEnablesStubs", 1.88],
  ["TestWireProviderUpdatesAndMockAppUpdateReplayToLateClient", 0.35],
  ["TestWireRealLMStudioNativeProviderTransport", 0],
  ["TestWireReconnectRestoresLiveSessionControlsAndPendingPermission", 1.72],
  ["TestWireSessionNoopSaveDoesNotBroadcastRefresh", 0.02],
  ["TestWireSessionRecoversTurnCompletedWithoutRenderer", 1.49],
  ["TestWireTraceAccessApprovalRevoke", 0.16],
  ["TestWireTraceAppChatSteer", 1.49],
  ["TestWireTraceChatEnvNumstat", 0.42],
  ["TestWireTraceForkChatSeedsPrefixAndDiverges", 0.82],
  ["TestWireTraceGroupedCatalogAndInterleavedProviders", 1.64],
  ["TestWireTraceHibernatedCheckpointKeepsTurnBaseline", 1.81],
  ["TestWireTraceMockCrashNeverReplaysAndNextDistinctPromptRuns", 0.83],
  ["TestWireTraceMockEngineCrashTerminalizesThenNextPromptResumesExactThread", 0.7],
  ["TestWireTraceMockPermissionTurn", 0.37],
  ["TestWireTraceNativeLocalProviderColdStartAndTurn", 0.78],
  ["TestWireTraceNotifyControllerOnlyRedactionAndNoTurnEndBacklog", 0.7],
  ["TestWireWorkspaceMoveCommitsBeforeInvalidationAndStaleReconnectUsesTargetCWD", 0.63],
  ["TestWorkassLanguageRuleIsAdjacentToEveryTurn", 0],
  ["TestWorkassModeEchoCannotReenterActorRefreshBeforeControlReply", 0.06],
  ["TestWorkassPromptsDoNotRestrictHostUIOrExternalBrowsers", 0],
  ["TestWorkassQuestionActorWireAnswerUnicodeIsolationAndReplay", 0.4],
  ["TestWorkassQuestionAdmissionRacesJobEndWithoutOrphaning", 0],
  ["TestWorkassQuestionAnswerReachesTheOwningRequester", 0],
  ["TestWorkassQuestionCallerAndAnswerValidationAreIsolated", 0],
  ["TestWorkassQuestionManagerBridgeUnit", 0.25],
  ["TestWorkassQuestionSchemaMutationClassificationAndMalformedInputs", 0],
  ["TestWorkassQuestionTimeoutAndTurnEndAreExplicit", 0.04],
  ["TestWorkspaceHandlersRequireExactPairBeforeCWDOrProviderWork", 0.04],
  ["TestWorkspaceMoveCreatesFreshEpochWithoutTranscriptReplay", 0.04],
  ["TestWorkspaceMoveRejectsActiveTurnBeforeCommit", 0.18],
  ["TestWorkspaceMoveWriteFailureKeepsLiveAndNativeBinding", 0.02],
  ["TestWorkspaceReturnCreatesCurrentRevisionLaneAndAcceptsNextTurn", 1.01],
]);
const DEFAULT_SECONDS = 0.25;
const MAX_BATCH_SECONDS = 4;
const MAX_BATCH_CASES = 6;
const HEAVY_PACKAGES = ['./internal/acp', './cmd/workass'];
// Startup probes retain their real readiness deadlines and run in one explicit
// serial batch until their fixtures can be isolated.
const SERIAL_TESTS = new Set([
  'TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession',
  'TestStartupDetectionDoesNotRetryDevinNeedsLogin',
]);

export function parseTestList(output) {
  const names = [];
  for (const line of output.split(/\r?\n/)) {
    const name = line.trim();
    if (/^(?:Test|Example|Fuzz)\S*$/.test(name)) names.push(name);
  }
  return names;
}

export function anchoredTestPattern(name) {
  return `^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`;
}

export function inspectCaseOutput(name, code, output) {
  const lines = output.split(/\r?\n/);
  const runs = [];
  const terminals = [];
  for (const line of lines) {
    const run = line.trim().match(/^=== RUN\s+(.+)$/);
    if (run) runs.push(run[1]);
    const terminal = line.match(/^\s*--- (PASS|SKIP|FAIL): (.+?)(?: \(.*\))?$/);
    if (terminal) terminals.push({ action: terminal[1].toLowerCase(), name: terminal[2] });
  }
  const rootPasses = terminals.filter(item => item.name === name && item.action === 'pass').length;
  const rootSkips = terminals.filter(item => item.name === name && item.action === 'skip').length;
  const rootFailures = terminals.filter(item => item.name === name && item.action === 'fail').length;
  const rootRuns = runs.filter(item => item === name).length;
  const nestedNames = runs.filter(item => item.startsWith(`${name}/`));
  const nestedTerminals = terminals.filter(item => item.name.startsWith(`${name}/`));
  const nestedTerminalCounts = new Map();
  for (const terminal of nestedTerminals) nestedTerminalCounts.set(terminal.name, (nestedTerminalCounts.get(terminal.name) ?? 0) + 1);
  const nestedOutcomes = {
    pass: nestedTerminals.filter(item => item.action === 'pass').length,
    skip: nestedTerminals.filter(item => item.action === 'skip').length,
    fail: nestedTerminals.filter(item => item.action === 'fail').length,
  };
  return {
    rootRuns, rootPasses, rootSkips, rootFailures,
    nestedRun: nestedNames.length, nestedPassed: nestedOutcomes.pass, nestedSkipped: nestedOutcomes.skip, nestedFailed: nestedOutcomes.fail,
    coverageError: rootRuns !== 1 || rootPasses + rootSkips + rootFailures !== 1 || (rootFailures === 0) !== (rootPasses + rootSkips === 1) || nestedNames.some(item => nestedTerminalCounts.get(item) !== 1) || nestedTerminals.some(item => !nestedNames.includes(item.name)),
    outcome: rootPasses ? 'pass' : rootSkips ? 'skip' : 'fail',
  };
}

export function partitionSerialCases(names, serialNames = SERIAL_TESTS) {
  return { serial: names.filter(name => serialNames.has(name)), parallel: names.filter(name => !serialNames.has(name)) };
}

export function partitionBatches(names, hints = HINTS, { maxWeight = MAX_BATCH_SECONDS, maxCases = MAX_BATCH_CASES } = {}) {
  const sorted = [...names].sort((a, b) => (hints.get(b) ?? DEFAULT_SECONDS) - (hints.get(a) ?? DEFAULT_SECONDS) || a.localeCompare(b));
  const batches = [];
  for (const name of sorted) {
    const weight = hints.get(name) ?? DEFAULT_SECONDS;
    let batch = batches.find(item => item.names.length < maxCases && item.weight + weight <= maxWeight);
    if (!batch) { batch = { names: [], weight: 0 }; batches.push(batch); }
    batch.names.push(name);
    batch.weight += weight;
  }
  return batches.map((batch, id) => ({ id, names: batch.names, weight: batch.weight }));
}

export function orderWorkByWeight(work, { slowPackage = 'workass/internal/machinebook' } = {}) {
  return [...work].sort((a, b) => Number(b.package === slowPackage) - Number(a.package === slowPackage) ||
    (b.weight ?? DEFAULT_SECONDS) - (a.weight ?? DEFAULT_SECONDS) ||
    String(a.package ?? '').localeCompare(String(b.package ?? '')) || String(a.id ?? '').localeCompare(String(b.id ?? '')));
}

function appendJsonLine(stream, value) { stream.write(`${JSON.stringify(value)}\n`); }

function spawnLogged(command, args, options, active) {
  return new Promise(resolve => {
    const started = performance.now();
    let stdout = '', stderr = '', spawnError = null;
    const child = (options.spawn ?? nodeSpawn)(command, args, {
      cwd: options.cwd, env: options.env ?? process.env,
      stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32',
    });
    active.add(child);
    child.stdout?.on('data', chunk => { stdout += chunk; });
    child.stderr?.on('data', chunk => { stderr += chunk; });
    child.on('error', error => { spawnError = error; });
    child.on('close', (code, signal) => {
      active.delete(child);
      resolve({ code: code ?? (spawnError ? 127 : 1), signal, spawnError: spawnError?.message ?? null, stdout, stderr, elapsedMs: performance.now() - started });
    });
  });
}

function signalGroup(child, signal) {
  try { if (process.platform === 'win32') child.kill(signal); else process.kill(-child.pid, signal); }
  catch { try { child.kill(signal); } catch {} }
}

async function stopChildren(active) {
  const children = [...active];
  for (const child of children) signalGroup(child, 'SIGTERM');
  let escalationTimer;
  await Promise.race([
    Promise.all(children.map(child => new Promise(resolve => child.once('close', resolve)))),
    new Promise(resolve => { escalationTimer = setTimeout(resolve, 1000); }),
  ]);
  clearTimeout(escalationTimer);
  for (const child of [...active]) signalGroup(child, 'SIGKILL');
  await Promise.all([...active].map(child => new Promise(resolve => child.once('close', resolve))));
}

function parseGoJson(output) {
  const events = [];
  for (const line of output.split(/\r?\n/)) { try { events.push(JSON.parse(line)); } catch {} }
  return events;
}

export async function runGoSuite({ cwd = process.cwd(), logDir, workers = 6, race = false, go = 'go', spawn = nodeSpawn, signalHandlers = true, abortSignal } = {}) {
  if (!Number.isInteger(workers) || workers < 1) throw new Error('workers must be a positive integer');
  workers = Math.min(workers, 6);
  const wallStart = performance.now();
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-matrix-'));
  const logs = logDir ?? path.join(os.tmpdir(), `workass-go-matrix-logs-${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}`);
  await mkdir(logs, { recursive: true });
  const runId = `${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}-${process.pid}-${randomUUID()}`;
  const jsonlPath = path.join(logs, `go-matrix-${runId}.jsonl`);
  const summaryPath = path.join(logs, `go-matrix-${runId}.json`);
  const jsonl = createWriteStream(jsonlPath, { flags: 'wx' });
  const active = new Set();
  let interrupted = null;
  let jsonlError = null;
  const onInterrupt = signal => { interrupted ??= signal; void stopChildren(active); };
  jsonl.on('error', error => { jsonlError ??= error; onInterrupt('LOG_ERROR'); });
  if (signalHandlers) { process.on('SIGINT', onInterrupt); process.on('SIGTERM', onInterrupt); }
  const onAbort = () => onInterrupt('ABORT');
  abortSignal?.addEventListener('abort', onAbort, { once: true });
  const commandTemp = async label => {
    const dir = path.join(root, `command-${label}`);
    await mkdir(dir, { recursive: true });
    return dir;
  };
  const envFor = dir => ({ ...process.env, TMPDIR: dir, TMP: dir, TEMP: dir });
  const run = async (command, args, label, commandCwd = cwd) => {
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    appendJsonLine(jsonl, { event: 'command-start', label, command, args, cwd: commandCwd, at: new Date().toISOString() });
    const tempDir = await commandTemp(label);
    const result = await spawnLogged(command, args, { cwd: commandCwd, spawn, env: envFor(tempDir) }, active);
    appendJsonLine(jsonl, { event: 'command-end', label, ...result });
    if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label, text: result.stdout });
    if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label, text: result.stderr });
    if (result.code !== 0) throw Object.assign(new Error(`${label} failed with exit ${result.code}${result.signal ? ` (${result.signal})` : ''}${result.spawnError ? `: ${result.spawnError}` : ''}`), { result, label });
    return result;
  };
  const summary = {
    ok: false, interrupted: null, cwd, workers, race,
    heavyPackages: {}, otherPackages: [], packageOutcomes: [],
    otherGo: { tests: 0, topLevelTests: 0, nestedTests: 0, passed: 0, failed: 0, skipped: 0 },
    elapsedMs: 0, jsonlPath, summaryPath,
  };
  try {
    const raceFlag = race ? ['-race'] : [];
    const buildAndList = await Promise.all(HEAVY_PACKAGES.map(async (pkg, index) => {
      const label = index === 0 ? 'acp' : 'workass';
      const binary = path.join(root, `${label}.test`);
      const packageCwd = path.join(cwd, pkg.replace(/^\.\//, ''));
      await run(go, ['test', ...raceFlag, '-c', '-o', binary, pkg], `compile-${label}`);
      const listing = await run(binary, ['-test.list', '.'], `list-${label}`, packageCwd);
      const names = parseTestList(listing.stdout);
      if (!names.length) throw new Error(`${pkg} test binary discovered no Test, Example, or Fuzz cases`);
      if (new Set(names).size !== names.length) throw new Error(`${pkg} test listing contains duplicate case names`);
      return { pkg, label, binary, packageCwd, names };
    }));
    const packages = (await run(go, ['list', './...'], 'list-packages')).stdout.trim().split(/\r?\n/).filter(Boolean);
    const heavyImportPaths = new Set(buildAndList.map(item => packages.find(pkg => pkg.endsWith(item.pkg.slice(1))) ?? `workass/${item.pkg.slice(2)}`));
    const otherPackages = packages.filter(pkg => !heavyImportPaths.has(pkg));
    summary.otherPackages = otherPackages;
    const batches = [];
    for (const item of buildAndList) {
      const { serial, parallel } = partitionSerialCases(item.names);
      item.serialCases = serial;
      item.batches = partitionBatches(parallel);
      summary.heavyPackages[item.pkg] = {
        discovered: item.names.length, serialCases: serial,
        batches: item.batches.map(batch => ({ id: batch.id, weight: Number(batch.weight.toFixed(2)), cases: batch.names.length })),
        cases: [], passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedPassed: 0, nestedFailed: 0, nestedSkipped: 0, serialElapsedMs: 0,
      };
      for (const batch of item.batches) batches.push({ ...batch, item, boundary: 'weighted' });
      for (const [serialIndex, name] of serial.entries()) batches.push({ id: `serial-${serialIndex}`, names: [name], weight: HINTS.get(name) ?? DEFAULT_SECONDS, item, boundary: 'serial' });
    }
    const workQueue = orderWorkByWeight([
      ...batches.map(batch => ({ kind: 'heavy', ...batch, package: packages.find(pkg => pkg.endsWith(batch.item.pkg.slice(1))) })),
      ...otherPackages.map(pkg => ({ kind: 'package', package: pkg, id: pkg, weight: pkg.endsWith('/machinebook') ? 10 : DEFAULT_SECONDS })),
    ]);
    const failures = [];
    let nextWork = 0;
    const executeBatch = async (batch, worker) => {
      if (interrupted) return;
      const { item } = batch;
      const tempDir = path.join(root, `worker-${worker}-${item.label}-batch-${batch.id}`);
      await mkdir(tempDir, { recursive: true });
      if (interrupted) return;
      const pattern = `^(?:${batch.names.map(name => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})$`;
      const result = await spawnLogged(item.binary, ['-test.v', `-test.run=${pattern}`, '-test.count=1', '-test.parallel=2'], { cwd: item.packageCwd, spawn, env: envFor(tempDir) }, active);
      const target = summary.heavyPackages[item.pkg];
      if (batch.boundary === 'serial') target.serialElapsedMs += result.elapsedMs;
      appendJsonLine(jsonl, { event: 'batch-end', package: item.pkg, batch: batch.id, worker, names: batch.names, elapsedMs: result.elapsedMs, code: result.code, signal: result.signal, spawnError: result.spawnError, stdout: result.stdout, stderr: result.stderr });
      for (const name of batch.names) {
        const inspected = inspectCaseOutput(name, result.code, result.stdout);
        const record = { package: item.pkg, name, worker, batch: batch.id, boundary: batch.boundary, cwd: item.packageCwd, elapsedMs: result.elapsedMs, code: result.code, signal: result.signal, spawnError: result.spawnError, ...inspected };
        target.cases.push(record);
        appendJsonLine(jsonl, { event: 'case', ...record });
        if (!inspected.coverageError && inspected.rootPasses === 1) target.passed++;
        else if (!inspected.coverageError && inspected.rootSkips === 1) target.skipped++;
        else target.failed++;
        target.nestedSkipped += inspected.nestedSkipped;
        target.nestedRun += inspected.nestedRun;
        target.nestedPassed += inspected.nestedPassed;
        target.nestedFailed += inspected.nestedFailed;
      }
      if (result.code !== 0) failures.push(`${item.pkg} batch ${batch.id} failed with exit ${result.code}`);
    };
    const executePackage = async (work, worker) => {
      const tempDir = await commandTemp(`other-${worker}-${work.package.replace(/[^a-zA-Z0-9_-]/g, '-')}`);
      const flags = ['test', ...raceFlag, '-json', '-count=1', '-p=1', '-parallel=2', work.package];
      appendJsonLine(jsonl, { event: 'command-start', label: `other-go-${work.package}`, command: go, args: flags, cwd, at: new Date().toISOString() });
      const result = await spawnLogged(go, flags, { cwd, spawn, env: envFor(tempDir) }, active);
      appendJsonLine(jsonl, { event: 'command-end', label: `other-go-${work.package}`, ...result });
      if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label: `other-go-${work.package}`, text: result.stdout });
      if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label: `other-go-${work.package}`, text: result.stderr });
      const events = parseGoJson(result.stdout);
      const outcomes = events.filter(event => ['pass', 'fail', 'skip'].includes(event.Action));
      let packageOutcome;
      for (const event of outcomes) {
        if (event.Test) {
          summary.otherGo.tests++;
          if (event.Test.includes('/')) summary.otherGo.nestedTests++;
          else summary.otherGo.topLevelTests++;
          summary.otherGo[event.Action === 'pass' ? 'passed' : event.Action === 'fail' ? 'failed' : 'skipped']++;
        } else if (event.Package) packageOutcome = event.Action;
      }
      if (!packageOutcome) failures.push(`${work.package} package result missing`);
      else summary.packageOutcomes.push({ package: work.package, action: packageOutcome });
      if (result.code !== 0) {
        summary.commandFailure ??= { label: `other-go-${work.package}`, code: result.code, signal: result.signal, spawnError: result.spawnError, stdout: result.stdout, stderr: result.stderr };
        failures.push(`${work.package} failed with exit ${result.code}`);
      }
    };
    const pool = Array.from({ length: Math.min(workers, workQueue.length) }, async (_, worker) => {
      while (!interrupted) {
        const index = nextWork++;
        if (index >= workQueue.length) return;
        const work = workQueue[index];
        if (work.kind === 'heavy') await executeBatch(work, worker);
        else await executePackage(work, worker);
      }
    });
    const settled = await Promise.allSettled(pool);
    const rejection = settled.find(item => item.status === 'rejected');
    if (rejection) throw rejection.reason;
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    for (const item of buildAndList) {
      const result = summary.heavyPackages[item.pkg];
      result.cases.sort((a, b) => a.name.localeCompare(b.name));
      if (result.cases.length !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: discovered ${result.discovered}, ran ${result.cases.length}`);
      if (new Set(result.cases.map(test => test.name)).size !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: duplicate execution`);
      if (result.cases.some(test => test.coverageError)) throw new Error(`${item.pkg} test output did not contain exactly one terminal outcome per discovered case`);
    }
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    if (summary.packageOutcomes.some(item => item.action === 'fail')) throw new Error('one or more remaining Go packages failed');
    if (failures.length) throw new Error(failures.join('; '));
    summary.ok = Object.values(summary.heavyPackages).every(item => item.failed === 0 && item.nestedFailed === 0) && summary.otherGo.failed === 0;
    if (!summary.ok) throw new Error('one or more Go cases failed or were missing');
  } catch (error) {
    summary.error = error.message;
    if (error.result) summary.commandFailure = { label: error.label, code: error.result.code, signal: error.result.signal, spawnError: error.result.spawnError, stdout: error.result.stdout, stderr: error.result.stderr };
    if (interrupted) summary.interrupted = interrupted;
    if (jsonlError) summary.logError = jsonlError.message;
    await stopChildren(active);
  } finally {
    if (signalHandlers) { process.off('SIGINT', onInterrupt); process.off('SIGTERM', onInterrupt); }
    abortSignal?.removeEventListener('abort', onAbort);
    await rm(root, { recursive: true, force: true });
    summary.elapsedMs = performance.now() - wallStart;
    if (!jsonlError) appendJsonLine(jsonl, { event: 'summary', ...summary });
    await new Promise(resolve => jsonl.end(resolve));
    await writeFile(summaryPath, `${JSON.stringify(summary, null, 2)}\n`);
  }
  return summary;
}

async function main() {
  const args = process.argv.slice(2);
  const options = { workers: 6, cwd: process.cwd() };
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--workers') options.workers = Number(args[++i]);
    else if (args[i] === '--cwd') options.cwd = path.resolve(args[++i]);
    else if (args[i] === '--log-dir') options.logDir = path.resolve(args[++i]);
    else if (args[i] === '--race') options.race = true;
    else if (args[i] === '--help') { process.stdout.write('Usage: node scripts/test-go-suite.mjs [--workers N] [--race] [--cwd DIR] [--log-dir DIR]\n'); return 0; }
    else throw new Error(`unknown argument: ${args[i]}`);
  }
  const summary = await runGoSuite(options);
  const packages = Object.values(summary.heavyPackages);
  const counts = packages.reduce((total, item) => ({ discovered: total.discovered + item.discovered, passed: total.passed + item.passed + item.nestedPassed, failed: total.failed + item.failed + item.nestedFailed, skipped: total.skipped + item.skipped + item.nestedSkipped, nestedRun: total.nestedRun + item.nestedRun, nestedSkipped: total.nestedSkipped + item.nestedSkipped }), { discovered: 0, passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedSkipped: 0 });
  counts.topLevelTests = counts.discovered + summary.otherGo.topLevelTests;
  counts.tests = counts.topLevelTests + counts.nestedRun + summary.otherGo.nestedTests;
  process.stdout.write(`${JSON.stringify({ ok: summary.ok, elapsedMs: Number(summary.elapsedMs.toFixed(1)), ...counts, otherGo: summary.otherGo, otherPackages: summary.otherPackages.length, jsonlPath: summary.jsonlPath, summaryPath: summary.summaryPath, error: summary.error }, null, 2)}\n`);
  return summary.ok ? 0 : 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().then(code => { process.exitCode = code; }).catch(error => { process.stderr.write(`${error.stack ?? error}\n`); process.exitCode = 1; });
}
