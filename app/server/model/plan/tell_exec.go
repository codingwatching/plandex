package plan

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"plandex-server/db"
	"plandex-server/hooks"
	"plandex-server/model"
	"plandex-server/types"

	shared "plandex-shared"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
)

func Tell(clients map[string]model.ClientInfo, plan *db.Plan, branch string, auth *types.ServerAuth, req *shared.TellPlanRequest) error {
	log.Printf("Tell: Called with plan ID %s on branch %s\n", plan.Id, branch)

	_, err := activatePlan(
		clients,
		plan,
		branch,
		auth,
		req.Prompt,
		false,
		req.AutoContext,
		req.SessionId,
	)

	if err != nil {
		log.Printf("Error activating plan: %v\n", err)
		return err
	}

	go execTellPlan(execTellPlanParams{
		clients:            clients,
		plan:               plan,
		branch:             branch,
		auth:               auth,
		req:                req,
		iteration:          0,
		shouldBuildPending: !req.IsChatOnly && req.BuildMode == shared.BuildModeAuto,
	})

	log.Printf("Tell: Tell operation completed successfully for plan ID %s on branch %s\n", plan.Id, branch)
	return nil
}

type execTellPlanParams struct {
	clients                    map[string]model.ClientInfo
	plan                       *db.Plan
	branch                     string
	auth                       *types.ServerAuth
	req                        *shared.TellPlanRequest
	iteration                  int
	missingFileResponse        shared.RespondMissingFileChoice
	shouldBuildPending         bool
	numErrorRetry              int
	unfinishedSubtaskReasoning string
}

func execTellPlan(params execTellPlanParams) {
	clients := params.clients
	plan := params.plan
	branch := params.branch
	auth := params.auth
	req := params.req
	iteration := params.iteration
	missingFileResponse := params.missingFileResponse
	shouldBuildPending := params.shouldBuildPending
	unfinishedSubtaskReasoning := params.unfinishedSubtaskReasoning

	log.Printf("[TellExec] Starting iteration %d for plan %s on branch %s", iteration, plan.Id, branch)
	currentUserId := auth.User.Id
	currentOrgId := auth.OrgId

	active := GetActivePlan(plan.Id, branch)

	if active == nil {
		log.Printf("execTellPlan: Active plan not found for plan ID %s on branch %s\n", plan.Id, branch)
		return
	}

	if missingFileResponse == "" {
		log.Println("Executing WillExecPlanHook")
		_, apiErr := hooks.ExecHook(hooks.WillExecPlan, hooks.HookParams{
			Auth: auth,
			Plan: plan,
		})

		if apiErr != nil {
			time.Sleep(100 * time.Millisecond)
			active.StreamDoneCh <- apiErr
			return
		}
	}

	planId := plan.Id
	log.Println("execTellPlan - Setting plan status to replying")
	err := db.SetPlanStatus(planId, branch, shared.PlanStatusReplying, "")
	if err != nil {
		log.Printf("Error setting plan %s status to replying: %v\n", planId, err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error setting plan status to replying",
		}

		log.Printf("execTellPlan: execTellPlan operation completed for plan ID %s on branch %s, iteration %d\n", plan.Id, branch, iteration)
		return
	}
	log.Println("execTellPlan - Plan status set to replying")

	state := &activeTellStreamState{
		modelStreamId:       active.ModelStreamId,
		execTellPlanParams:  params,
		clients:             clients,
		req:                 req,
		auth:                auth,
		currentOrgId:        currentOrgId,
		currentUserId:       currentUserId,
		plan:                plan,
		branch:              branch,
		iteration:           iteration,
		missingFileResponse: missingFileResponse,
	}

	log.Println("execTellPlan - Loading tell plan")
	err = state.loadTellPlan()
	if err != nil {
		return
	}
	log.Println("execTellPlan - Tell plan loaded")

	activatePaths, activatePathsOrdered := state.resolveCurrentStage()

	var tentativeModelConfig shared.ModelRoleConfig
	var tentativeMaxTokens int
	if state.currentStage.TellStage == shared.TellStagePlanning {
		if state.currentStage.PlanningPhase == shared.PlanningPhaseContext {
			log.Println("Tell plan - isContextStage - setting modelConfig to context loader")
			tentativeModelConfig = state.settings.ModelPack.GetArchitect()
			tentativeMaxTokens = state.settings.GetArchitectEffectiveMaxTokens()
		} else {
			plannerConfig := state.settings.ModelPack.Planner
			tentativeModelConfig = plannerConfig.ModelRoleConfig
			tentativeMaxTokens = state.settings.GetPlannerEffectiveMaxTokens()
		}
	} else if state.currentStage.TellStage == shared.TellStageImplementation {
		tentativeModelConfig = state.settings.ModelPack.GetCoder()
		tentativeMaxTokens = tentativeModelConfig.GetFinalLargeContextFallback().BaseModelConfig.MaxTokens
	} else {
		log.Printf("Tell plan - execTellPlan - unknown tell stage: %s\n", state.currentStage.TellStage)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Unknown tell stage",
		}
		return
	}
	state.tenativeModelConfig = &tentativeModelConfig

	ok, tokensWithoutContext := state.dryRunCalculateTokensWithoutContext(tentativeMaxTokens, unfinishedSubtaskReasoning)
	if !ok {
		return
	}

	var planStageSharedMsgs []*types.ExtendedChatMessagePart
	var planningPhaseOnlyMsgs []*types.ExtendedChatMessagePart
	var implementationMsgs []*types.ExtendedChatMessagePart

	if state.currentStage.TellStage == shared.TellStageImplementation {
		implementationMsgs = state.formatModelContext(formatModelContextParams{
			includeMaps:         false,
			smartContextEnabled: req.SmartContext,
			includeApplyScript:  req.ExecEnabled,
		})
	} else if state.currentStage.TellStage == shared.TellStagePlanning {
		// add the shared context between planning and context phases first so it can be cached
		// this is just for the map and any manually loaded contexts - auto contexts will be added later
		planStageSharedMsgs = state.formatModelContext(formatModelContextParams{
			includeMaps:         true,
			smartContextEnabled: req.SmartContext,
			includeApplyScript:  req.ExecEnabled,
			baseOnly:            true,
			cacheControl:        true,
		})

		if state.currentStage.PlanningPhase == shared.PlanningPhaseTasks {
			if req.AutoContext {
				msg := types.ExtendedChatMessage{
					Role:    openai.ChatMessageRoleSystem,
					Content: []types.ExtendedChatMessagePart{},
				}
				for _, part := range planStageSharedMsgs {
					msg.Content = append(msg.Content, *part)
				}
				sharedMsgsTokens := model.GetMessagesTokenEstimate(msg)

				tokensRemaining := tentativeMaxTokens - (sharedMsgsTokens + tokensWithoutContext)

				if tokensRemaining < 0 {
					log.Println("tokensRemaining is negative")
					active.StreamDoneCh <- &shared.ApiError{
						Type:   shared.ApiErrorTypeOther,
						Status: http.StatusInternalServerError,
						Msg:    "Max tokens exceeded before adding context",
					}
					return
				}

				planningPhaseOnlyMsgs = state.formatModelContext(formatModelContextParams{
					includeMaps:          false,
					smartContextEnabled:  req.SmartContext,
					includeApplyScript:   false, // already included in planStageSharedMsgs
					activeOnly:           true,
					activatePaths:        activatePaths,
					activatePathsOrdered: activatePathsOrdered,
					maxTokens:            int(float64(tokensRemaining) * 0.95), // leave a little extra room
				})
			} else {
				// if auto context is disabled, just dump in any remaining auto contexts, since all basic contexts have already been added in planStageSharedMsgs
				planningPhaseOnlyMsgs = state.formatModelContext(formatModelContextParams{
					includeMaps:         false,
					smartContextEnabled: req.SmartContext,
					includeApplyScript:  false, // already included in planStageSharedMsgs
					autoOnly:            true,
				})
			}
		}
	}

	getTellSysPromptParams := getTellSysPromptParams{
		planStageSharedMsgs:   planStageSharedMsgs,
		planningPhaseOnlyMsgs: planningPhaseOnlyMsgs,
		implementationMsgs:    implementationMsgs,
		contextTokenLimit:     tentativeMaxTokens,
	}

	// log.Println("getTellSysPromptParams:\n", spew.Sdump(getTellSysPromptParams))

	sysParts, err := state.getTellSysPrompt(getTellSysPromptParams)
	if err != nil {
		log.Printf("Error getting tell sys prompt: %v\n", err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    err.Error(),
		}
		return
	}

	// log.Println("**sysPrompt:**\n", spew.Sdump(sysParts))

	state.messages = []types.ExtendedChatMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: sysParts,
		},
	}

	promptMessage, ok := state.resolvePromptMessage(unfinishedSubtaskReasoning)
	if !ok {
		return
	}

	// log.Println("messages:\n\n", spew.Sdump(state.messages))

	// log.Println("promptMessage:", spew.Sdump(promptMessage))

	state.tokensBeforeConvo =
		model.GetMessagesTokenEstimate(state.messages...) +
			model.GetMessagesTokenEstimate(*promptMessage) +
			state.latestSummaryTokens +
			model.TokensPerRequest

	// print out breakdown of token usage
	log.Printf("Latest summary tokens: %d\n", state.latestSummaryTokens)
	log.Printf("Total tokens before convo: %d\n", state.tokensBeforeConvo)

	if state.tokensBeforeConvo > state.settings.GetPlannerEffectiveMaxTokens() {
		// token limit already exceeded before adding conversation
		err := fmt.Errorf("token limit exceeded before adding conversation")
		log.Printf("Error: %v\n", err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Token limit exceeded before adding conversation",
		}
		return
	}

	if !state.addConversationMessages() {
		return
	}

	// add the prompt message to the end of the messages slice
	if promptMessage != nil {
		state.messages = append(state.messages, *promptMessage)
	} else {
		log.Println("promptMessage is nil")
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Prompt message isn't set",
		}
		return
	}

	state.replyId = uuid.New().String()
	state.replyParser = types.NewReplyParser()

	if missingFileResponse == "" {
		state.messages = append(state.messages, *promptMessage)
	} else if !state.handleMissingFileResponse(unfinishedSubtaskReasoning) {
		return
	}

	log.Printf("\n\nMessages: %d\n", len(state.messages))
	// for _, message := range state.messages {
	// 	log.Printf("%s: %v\n", message.Role, message.Content)
	// }

	requestTokens := model.GetMessagesTokenEstimate(state.messages...) + model.TokensPerRequest
	state.totalRequestTokens = requestTokens

	stop := []string{"<PlandexFinish/>"}
	modelConfig := tentativeModelConfig

	log.Println("Tell plan - setting modelConfig")
	log.Println("Tell plan - requestTokens:", requestTokens)
	log.Println("Tell plan - state.currentStage.TellStage:", state.currentStage.TellStage)
	log.Println("Tell plan - state.currentStage.PlanningPhase:", state.currentStage.PlanningPhase)

	if state.currentStage.TellStage == shared.TellStagePlanning {
		if state.currentStage.PlanningPhase == shared.PlanningPhaseContext {
			log.Println("Tell plan - isContextStage - setting modelConfig to context loader")
			modelConfig = state.settings.ModelPack.GetArchitect().GetRoleForInputTokens(requestTokens)
			log.Println("Tell plan - got modelConfig for context phase")
		} else if state.currentStage.PlanningPhase == shared.PlanningPhaseTasks {
			modelConfig = state.settings.ModelPack.Planner.GetRoleForInputTokens(requestTokens).ModelRoleConfig
			log.Println("Tell plan - got modelConfig for tasks phase")
		}
	} else if state.currentStage.TellStage == shared.TellStageImplementation {
		modelConfig = state.settings.ModelPack.GetCoder().GetRoleForInputTokens(requestTokens)
		log.Println("Tell plan - got modelConfig for implementation stage")
	}

	log.Println("Tell plan - modelConfig:", spew.Sdump(modelConfig))

	// if the model doesn't support cache control, remove the cache control spec from the messages
	if !modelConfig.BaseModelConfig.SupportsCacheControl {
		for i := range state.messages {
			for j := range state.messages[i].Content {
				if state.messages[i].Content[j].CacheControl != nil {
					state.messages[i].Content[j].CacheControl = nil
				}
			}
		}
	}

	// if the model doesn't support images, remove any image parts from the messages
	if !modelConfig.BaseModelConfig.HasImageSupport {
		log.Println("Tell exec - model doesn't support images. Removing image parts from messages. File name will still be included.")

		for i := range state.messages {
			filteredContent := []types.ExtendedChatMessagePart{}
			for _, part := range state.messages[i].Content {
				if part.Type != openai.ChatMessagePartTypeImageURL {
					filteredContent = append(filteredContent, part)
				}
			}
			state.messages[i].Content = filteredContent
		}
	}

	log.Println("tell exec - will send model request with:", spew.Sdump(map[string]interface{}{
		"provider": modelConfig.BaseModelConfig.Provider,
		"model":    modelConfig.BaseModelConfig.ModelName,
		"tokens":   requestTokens,
	}))

	_, apiErr := hooks.ExecHook(hooks.WillSendModelRequest, hooks.HookParams{
		Auth: auth,
		Plan: plan,
		WillSendModelRequestParams: &hooks.WillSendModelRequestParams{
			InputTokens:  requestTokens,
			OutputTokens: modelConfig.BaseModelConfig.MaxOutputTokens - requestTokens,
			ModelName:    modelConfig.BaseModelConfig.ModelName,
		},
	})
	if apiErr != nil {
		active.StreamDoneCh <- apiErr
		return
	}

	// log.Println("Stop:", stop)
	// spew.Dump(state.messages)

	// log.Println("modelConfig:", spew.Sdump(modelConfig))

	modelReq := types.ExtendedChatCompletionRequest{
		Model:    modelConfig.BaseModelConfig.ModelName,
		Messages: state.messages,
		Stream:   true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
		Temperature: modelConfig.Temperature,
		TopP:        modelConfig.TopP,
		Stop:        stop,
	}

	state.requestStartedAt = time.Now()
	state.originalReq = &modelReq
	state.modelConfig = &modelConfig

	// output the modelReq to a json file
	// if jsonData, err := json.MarshalIndent(modelReq, "", "  "); err == nil {
	// 	timestamp := time.Now().Format("2006-01-02-150405")
	// 	filename := fmt.Sprintf("generations/model-request-%s.json", timestamp)
	// 	if err := os.WriteFile(filename, jsonData, 0644); err != nil {
	// 		log.Printf("Error writing model request to file: %v\n", err)
	// 	}
	// } else {
	// 	log.Printf("Error marshaling model request to JSON: %v\n", err)
	// }

	stream, err := model.CreateChatCompletionStream(clients, &modelConfig, active.ModelStreamCtx, modelReq)
	if err != nil {
		log.Printf("Error starting reply stream: %v\n", err)

		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error starting reply stream: " + err.Error(),
		}
		return
	}

	if shouldBuildPending {
		go state.queuePendingBuilds()
	}

	UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
		ap.CurrentStreamingReplyId = state.replyId
		ap.CurrentReplyDoneCh = make(chan bool, 1)
	})

	go state.listenStream(stream)
}

func (state *activeTellStreamState) dryRunCalculateTokensWithoutContext(tentativeMaxTokens int, unfinishedSubtaskReasoning string) (bool, int) {
	clone := &activeTellStreamState{
		modelStreamId:       state.modelStreamId,
		execTellPlanParams:  state.execTellPlanParams,
		clients:             state.clients,
		req:                 state.req,
		auth:                state.auth,
		currentOrgId:        state.currentOrgId,
		currentUserId:       state.currentUserId,
		plan:                state.plan,
		branch:              state.branch,
		iteration:           state.iteration,
		missingFileResponse: state.missingFileResponse,
		settings:            state.settings,
		currentStage:        state.currentStage,
		subtasks:            state.subtasks,
		currentSubtask:      state.currentSubtask,
		convo:               state.convo,
		summaries:           state.summaries,
		latestSummaryTokens: state.latestSummaryTokens,
		userPrompt:          state.userPrompt,
		promptMessage:       state.promptMessage,
		hasContextMap:       state.hasContextMap,
		contextMapEmpty:     state.contextMapEmpty,
		hasAssistantReply:   state.hasAssistantReply,
		modelContext:        state.modelContext,
		activePlan:          state.activePlan,
		tenativeModelConfig: state.tenativeModelConfig,
	}

	sysParts, err := clone.getTellSysPrompt(getTellSysPromptParams{
		contextTokenLimit:    tentativeMaxTokens,
		dryRunWithoutContext: true,
	})

	if err != nil {
		log.Printf("error getting tell sys prompt for dry run token calculation: %v", err)
		state.activePlan.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error getting tell sys prompt for dry run token calculation",
		}
		return false, 0
	}

	clone.messages = []types.ExtendedChatMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: sysParts,
		},
	}

	promptMessage, ok := clone.resolvePromptMessage(unfinishedSubtaskReasoning)
	if !ok {
		return false, 0
	}

	clone.tokensBeforeConvo =
		model.GetMessagesTokenEstimate(clone.messages...) +
			model.GetMessagesTokenEstimate(*promptMessage) +
			clone.latestSummaryTokens +
			model.TokensPerRequest

	if clone.tokensBeforeConvo > clone.settings.GetPlannerEffectiveMaxTokens() {
		log.Println("tokensBeforeConvo exceeds max tokens during dry run")
		state.activePlan.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Max tokens exceeded before adding conversation",
		}
		return false, 0
	}

	if !clone.addConversationMessages() {
		return false, 0
	}

	clone.messages = append(clone.messages, *promptMessage)

	return true, model.GetMessagesTokenEstimate(clone.messages...) + model.TokensPerRequest
}
