// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compat_oai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

// mapToStruct unmarshals a map[string]any to the expected config type.
func mapToStruct(m map[string]any, v any) error {
	jsonData, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, v)
}

// ModelGenerator handles OpenAI generation requests
type ModelGenerator struct {
	client     *openai.Client
	modelName  string
	request    *openai.ChatCompletionNewParams
	messages   []openai.ChatCompletionMessageParamUnion
	tools      []openai.ChatCompletionToolParam
	toolChoice openai.ChatCompletionToolChoiceOptionUnionParam
	// Store any errors that occur during building
	err error
}

func (g *ModelGenerator) GetRequest() *openai.ChatCompletionNewParams {
	return g.request
}

// NewModelGenerator creates a new ModelGenerator instance
func NewModelGenerator(client *openai.Client, modelName string) *ModelGenerator {
	return &ModelGenerator{
		client:    client,
		modelName: modelName,
		request: &openai.ChatCompletionNewParams{
			Model: (modelName),
		},
	}
}

// WithMessages adds messages to the request
func (g *ModelGenerator) WithMessages(messages []*ai.Message) *ModelGenerator {
	// Return early if we already have an error
	if g.err != nil {
		return g
	}

	if messages == nil {
		return g
	}

	oaiMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, msg := range messages {
		content := g.concatenateContent(msg.Content)
		switch msg.Role {
		case ai.RoleSystem:
			oaiMessages = append(oaiMessages, openai.SystemMessage(content))
		case ai.RoleModel:
			oaiMessages = append(oaiMessages, openai.AssistantMessage(content))

			am := openai.ChatCompletionAssistantMessageParam{}
			if msg.Content[0].Text != "" {
				am.Content.OfArrayOfContentParts = append(am.Content.OfArrayOfContentParts, openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{
					OfText: &openai.ChatCompletionContentPartTextParam{
						Text: msg.Content[0].Text,
					},
				})
			}
			toolCalls := convertToolCalls(msg.Content)
			if len(toolCalls) > 0 {
				am.ToolCalls = (toolCalls)
			}
			oaiMessages = append(oaiMessages, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &am,
			})
		case ai.RoleTool:
			for _, p := range msg.Content {
				if !p.IsToolResponse() {
					continue
				}
				// Use the captured tool call ID (Ref) if available, otherwise fall back to tool name
				toolCallID := p.ToolResponse.Ref
				if toolCallID == "" {
					toolCallID = p.ToolResponse.Name
				}

				// OpenAI SDK ToolMessage expects (content, toolCallID) in this version.
				tm := openai.ToolMessage(
					anyToJSONString(p.ToolResponse.Output),
					toolCallID,
				)
				oaiMessages = append(oaiMessages, tm)
			}
		case ai.RoleUser:
			oaiMessages = append(oaiMessages, openai.UserMessage(content))

			parts := []openai.ChatCompletionContentPartUnionParam{}
			for _, p := range msg.Content {
				if p.IsMedia() {
					// If middleware downloaded non-image media, it will be a data: URI.
					if strings.HasPrefix(p.Text, "data:") && !strings.HasPrefix(strings.ToLower(p.ContentType), "image/") {
						if filePart, ok := buildFilePartFromDataURI(p.Text, p.ContentType); ok {
							parts = append(parts, filePart)
							continue
						}
					}
					// Images: let API fetch or accept data URL directly as image content part
					part := openai.ImageContentPart(
						openai.ChatCompletionContentPartImageImageURLParam{
							URL: p.Text,
						})
					parts = append(parts, part)
					continue
				}
			}
			if len(parts) > 0 {
				oaiMessages = append(oaiMessages, openai.ChatCompletionMessageParamUnion{
					OfUser: &openai.ChatCompletionUserMessageParam{
						Content: openai.ChatCompletionUserMessageParamContentUnion{OfArrayOfContentParts: parts},
					},
				})
			}
		default:
			// ignore parts from not supported roles
			continue
		}

	}
	g.messages = oaiMessages
	return g
}

// WithConfig adds configuration parameters from the model request
// see https://platform.openai.com/docs/api-reference/responses/create
// for more details on openai's request fields
func (g *ModelGenerator) WithConfig(config any) *ModelGenerator {
	// Return early if we already have an error
	if g.err != nil {
		return g
	}

	if config == nil {
		return g
	}

	var openaiConfig openai.ChatCompletionNewParams
	switch cfg := config.(type) {
	case openai.ChatCompletionNewParams:
		openaiConfig = cfg
	case *openai.ChatCompletionNewParams:
		openaiConfig = *cfg
	case map[string]any:
		if err := mapToStruct(cfg, &openaiConfig); err != nil {
			g.err = fmt.Errorf("failed to convert config to OpenAIConfig: %w", err)
			return g
		}
	default:
		g.err = fmt.Errorf("unexpected config type: %T", config)
		return g
	}

	// keep the original model in the updated config structure
	openaiConfig.Model = g.request.Model
	g.request = &openaiConfig
	return g
}

// WithTools adds tools to the request
func (g *ModelGenerator) WithTools(tools []*ai.ToolDefinition) *ModelGenerator {
	if g.err != nil {
		return g
	}

	if tools == nil {
		return g
	}

	toolParams := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			continue
		}

		toolParams = append(toolParams, openai.ChatCompletionToolParam{
			Function: (shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  openai.FunctionParameters(tool.InputSchema),
				Strict:      openai.Bool(false), // TODO: implement strict mode
			}),
		})
	}

	// Set the tools in the request
	// If no tools are provided, set it to nil
	// This is important to avoid sending an empty array in the request
	// which is not supported by some vendor APIs
	if len(toolParams) > 0 {
		g.tools = toolParams
	}

	return g
}

// Generate executes the generation request
func (g *ModelGenerator) Generate(ctx context.Context, handleChunk func(context.Context, *ai.ModelResponseChunk) error) (*ai.ModelResponse, error) {
	// Check for any errors that occurred during building
	if g.err != nil {
		return nil, g.err
	}

	if len(g.messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	g.request.Messages = (g.messages)

	if len(g.tools) > 0 {
		g.request.Tools = (g.tools)
	}

	if handleChunk != nil {
		return g.generateStream(ctx, handleChunk)
	}
	return g.generateComplete(ctx)
}

// concatenateContent concatenates text content into a single string
func (g *ModelGenerator) concatenateContent(parts []*ai.Part) string {
	content := ""
	for _, part := range parts {
		content += part.Text
	}
	return content
}

// generateStream generates a streaming model response
func (g *ModelGenerator) generateStream(ctx context.Context, handleChunk func(context.Context, *ai.ModelResponseChunk) error) (*ai.ModelResponse, error) {
	stream := g.client.Chat.Completions.NewStreaming(ctx, *g.request)
	defer stream.Close()

	var fullResponse ai.ModelResponse
	fullResponse.Message = &ai.Message{
		Role:    ai.RoleModel,
		Content: make([]*ai.Part, 0),
	}

	// Initialize request and usage
	fullResponse.Request = &ai.ModelRequest{}
	fullResponse.Usage = &ai.GenerationUsage{
		InputTokens:  0,
		OutputTokens: 0,
		TotalTokens:  0,
	}

	var currentToolCall *ai.ToolRequest
	var currentArguments string

	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]

			switch choice.FinishReason {
			case "tool_calls", "stop":
				fullResponse.FinishReason = ai.FinishReasonStop
			case "length":
				fullResponse.FinishReason = ai.FinishReasonLength
			case "content_filter":
				fullResponse.FinishReason = ai.FinishReasonBlocked
			case "function_call":
				fullResponse.FinishReason = ai.FinishReasonOther
			default:
				fullResponse.FinishReason = ai.FinishReasonUnknown
			}

			// handle tool calls
			for _, toolCall := range choice.Delta.ToolCalls {
				// first tool call (= current tool call is nil) contains the tool call name
				if currentToolCall == nil {
					currentToolCall = &ai.ToolRequest{
						Name: toolCall.Function.Name,
					}
				}

				if toolCall.Function.Arguments != "" {
					currentArguments += toolCall.Function.Arguments
				}
			}

			// when tool call is complete
			if choice.FinishReason == "tool_calls" && currentToolCall != nil {
				// parse accumulated arguments string
				if currentArguments != "" {
					currentToolCall.Input = jsonStringToMap(currentArguments)
				}

				fullResponse.Message.Content = []*ai.Part{ai.NewToolRequestPart(currentToolCall)}
				return &fullResponse, nil
			}

			content := chunk.Choices[0].Delta.Content
			modelChunk := &ai.ModelResponseChunk{
				Content: []*ai.Part{ai.NewTextPart(content)},
			}

			if err := handleChunk(ctx, modelChunk); err != nil {
				return nil, fmt.Errorf("callback error: %w", err)
			}

			fullResponse.Message.Content = append(fullResponse.Message.Content, modelChunk.Content...)

			// Update Usage
			fullResponse.Usage.InputTokens += int(chunk.Usage.PromptTokens)
			fullResponse.Usage.OutputTokens += int(chunk.Usage.CompletionTokens)
			fullResponse.Usage.TotalTokens += int(chunk.Usage.TotalTokens)
		}
	}

	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream error: %w", err)
	}

	return &fullResponse, nil
}

// generateComplete generates a complete model response
func (g *ModelGenerator) generateComplete(ctx context.Context) (*ai.ModelResponse, error) {
	completion, err := g.client.Chat.Completions.New(ctx, *g.request)
	if err != nil {
		return nil, fmt.Errorf("failed to create completion: %w", err)
	}

	resp := &ai.ModelResponse{
		Request: &ai.ModelRequest{},
		Usage: &ai.GenerationUsage{
			InputTokens:  int(completion.Usage.PromptTokens),
			OutputTokens: int(completion.Usage.CompletionTokens),
			TotalTokens:  int(completion.Usage.TotalTokens),
		},
		Message: &ai.Message{
			Role: ai.RoleModel,
		},
	}

	choice := completion.Choices[0]

	switch choice.FinishReason {
	case "stop", "tool_calls":
		resp.FinishReason = ai.FinishReasonStop
	case "length":
		resp.FinishReason = ai.FinishReasonLength
	case "content_filter":
		resp.FinishReason = ai.FinishReasonBlocked
	case "function_call":
		resp.FinishReason = ai.FinishReasonOther
	default:
		resp.FinishReason = ai.FinishReasonUnknown
	}

	// handle tool calls
	var toolRequestParts []*ai.Part
	for _, toolCall := range choice.Message.ToolCalls {
		toolRequestParts = append(toolRequestParts, ai.NewToolRequestPart(&ai.ToolRequest{
			Ref:   toolCall.ID,
			Name:  toolCall.Function.Name,
			Input: jsonStringToMap(toolCall.Function.Arguments),
		}))
	}
	if len(toolRequestParts) > 0 {
		resp.Message.Content = toolRequestParts
		return resp, nil
	}

	resp.Message.Content = []*ai.Part{
		ai.NewTextPart(completion.Choices[0].Message.Content),
	}
	return resp, nil
}

func convertToolCalls(content []*ai.Part) []openai.ChatCompletionMessageToolCallParam {
	var toolCalls []openai.ChatCompletionMessageToolCallParam
	for _, p := range content {
		if !p.IsToolRequest() {
			continue
		}
		toolCall := convertToolCall(p)
		toolCalls = append(toolCalls, toolCall)
	}
	return toolCalls
}

func convertToolCall(part *ai.Part) openai.ChatCompletionMessageToolCallParam {
	toolCallID := part.ToolRequest.Ref
	if toolCallID == "" {
		toolCallID = part.ToolRequest.Name
	}

	param := openai.ChatCompletionMessageToolCallParam{
		ID: (toolCallID),
		Function: (openai.ChatCompletionMessageToolCallFunctionParam{
			Name: (part.ToolRequest.Name),
		}),
	}

	if part.ToolRequest.Input != nil {
		param.Function.Arguments = (anyToJSONString(part.ToolRequest.Input))
	}

	return param
}

func jsonStringToMap(jsonString string) map[string]any {
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonString), &result); err != nil {
		panic(fmt.Errorf("unmarshal failed to parse json string %s: %w", jsonString, err))
	}
	return result
}

func anyToJSONString(data any) string {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		panic(fmt.Errorf("failed to marshal any to JSON string: data, %#v %w", data, err))
	}
	return string(jsonBytes)
}

// buildFilePartFromDataURI converts a data: URI into an OpenAI file content part.
// Expects content like: data:<mime>;base64,<payload>
func buildFilePartFromDataURI(dataURI string, fallbackContentType string) (openai.ChatCompletionContentPartUnionParam, bool) {
	if !strings.HasPrefix(dataURI, "data:") {
		return openai.ChatCompletionContentPartUnionParam{}, false
	}
	comma := strings.Index(dataURI, ",")
	if comma < 0 {
		return openai.ChatCompletionContentPartUnionParam{}, false
	}
	header := dataURI[:comma]

	contentType := fallbackContentType
	if semi := strings.Index(header, ";"); semi > 5 {
		contentType = header[len("data:"):semi]
	} else if len(header) > len("data:") {
		contentType = header[len("data:"):]
	}

	filename := "attachment"
	if strings.Contains(strings.ToLower(contentType), "pdf") {
		filename = "document.pdf"
	}

	return openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
		FileData: openai.String(dataURI),
		Filename: openai.String(filename),
	}), true
}
