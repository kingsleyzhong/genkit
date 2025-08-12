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
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

var (
	// BasicText describes model capabilities for text-only GPT models.
	BasicText = ai.ModelSupports{
		Multiturn:  true,
		Tools:      true,
		SystemRole: true,
		Media:      false,
	}

	// Multimodal describes model capabilities for multimodal GPT models.
	Multimodal = ai.ModelSupports{
		Multiturn:  true,
		Tools:      true,
		SystemRole: true,
		Media:      true,
		ToolChoice: true,
	}
)

// OpenAICompatible is a plugin that provides compatibility with OpenAI's Compatible APIs.
// It allows defining models and embedders that can be used with Genkit.
type OpenAICompatible struct {
	// mu protects concurrent access to the client and initialization state
	mu sync.Mutex

	// initted tracks whether the plugin has been initialized
	initted bool

	// client is the OpenAI client used for making API requests
	// see https://github.com/openai/openai-go
	client *openai.Client

	// Opts contains request options for the OpenAI client.
	// Required: Must include at least WithAPIKey for authentication.
	// Optional: Can include other options like WithOrganization, WithBaseURL, etc.
	Opts []option.RequestOption

	// Provider is a unique identifier for the plugin.
	// This will be used as a prefix for model names (e.g., "myprovider/model-name").
	// Should be lowercase and match the plugin's Name() method.
	Provider string
}

// Init implements genkit.Plugin.
func (o *OpenAICompatible) Init(ctx context.Context, g *genkit.Genkit) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.initted {
		return errors.New("compat_oai.Init already called")
	}

	// create client
	client := openai.NewClient(o.Opts...)
	o.client = &client
	o.initted = true

	return nil
}

// Name implements genkit.Plugin.
func (o *OpenAICompatible) Name() string {
	return o.Provider
}

// DefineModel defines a model in the registry
func (o *OpenAICompatible) DefineModel(g *genkit.Genkit, provider, name string, info ai.ModelInfo) (ai.Model, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.initted {
		return nil, errors.New("OpenAICompatible.Init not called")
	}

	// Strip provider prefix if present to check against supportedModels
	modelName := strings.TrimPrefix(name, provider+"/")

	// Base handler
	handler := func(
		ctx context.Context,
		input *ai.ModelRequest,
		cb func(context.Context, *ai.ModelResponseChunk) error,
	) (*ai.ModelResponse, error) {
		generator := NewModelGenerator(o.client, modelName).
			WithMessages(input.Messages).
			WithConfig(input.Config).
			WithTools(input.Tools)

		return generator.Generate(ctx, cb)
	}

	// If media is supported, pre-download non-image media so we can send as file content parts
	if info.Supports != nil && info.Supports.Media {
		handler = core.ChainMiddleware(ai.DownloadRequestMedia(&ai.DownloadMediaOptions{
			MaxBytes: 1024 * 1024 * 20, // 20MB
			Filter: func(part *ai.Part) bool {
				// Only download media parts
				if !part.IsMedia() {
					return false
				}
				// If content type known, skip images
				if ct := strings.ToLower(part.ContentType); ct != "" {
					return !strings.HasPrefix(ct, "image/")
				}
				// Otherwise, infer from URL extension; download if not an image
				u, err := url.Parse(part.Text)
				if err != nil {
					return true
				}
				ext := strings.ToLower(path.Ext(u.Path))
				switch ext {
				case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".tiff", ".tif", ".heic", ".heif":
					return false
				default:
					return true
				}
			},
		}))(handler)
	}

	return genkit.DefineModel(g, provider, name, &info, handler), nil
}

// DefineEmbedder defines an embedder with a given name.
func (o *OpenAICompatible) DefineEmbedder(g *genkit.Genkit, provider, name string, embedOpts *ai.EmbedderOptions) (ai.Embedder, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.initted {
		return nil, errors.New("OpenAICompatible.Init not called")
	}

	return genkit.DefineEmbedder(g, provider, name, embedOpts, func(ctx context.Context, input *ai.EmbedRequest) (*ai.EmbedResponse, error) {
		var data openai.EmbeddingNewParamsInputUnion
		for _, doc := range input.Input {
			for _, p := range doc.Content {
				data.OfArrayOfStrings = append(data.OfArrayOfStrings, p.Text)
			}
		}

		params := openai.EmbeddingNewParams{
			Input:          openai.EmbeddingNewParamsInputUnion(data),
			Model:          name,
			EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
		}

		embeddingResp, err := o.client.Embeddings.New(ctx, params)
		if err != nil {
			return nil, err
		}

		resp := &ai.EmbedResponse{}
		for _, emb := range embeddingResp.Data {
			embedding := make([]float32, len(emb.Embedding))
			for i, val := range emb.Embedding {
				embedding[i] = float32(val)
			}
			resp.Embeddings = append(resp.Embeddings, &ai.Embedding{Embedding: embedding})
		}
		return resp, nil
	}), nil
}

// IsDefinedEmbedder reports whether the named [Embedder] is defined by this plugin.
func (o *OpenAICompatible) IsDefinedEmbedder(g *genkit.Genkit, name string, provider string) bool {
	return genkit.LookupEmbedder(g, provider, name) != nil
}

// Embedder returns the [ai.Embedder] with the given name.
// It returns nil if the embedder was not defined.
func (o *OpenAICompatible) Embedder(g *genkit.Genkit, name string, provider string) ai.Embedder {
	return genkit.LookupEmbedder(g, provider, name)
}

// Model returns the [ai.Model] with the given name.
// It returns nil if the model was not defined.
func (o *OpenAICompatible) Model(g *genkit.Genkit, name string, provider string) ai.Model {
	return genkit.LookupModel(g, provider, name)
}

// IsDefinedModel reports whether the named [Model] is defined by this plugin.
func (o *OpenAICompatible) IsDefinedModel(g *genkit.Genkit, name string, provider string) bool {
	return genkit.LookupModel(g, provider, name) != nil
}

func (o *OpenAICompatible) ListActions(ctx context.Context) []core.ActionDesc {
	actions := []core.ActionDesc{}

	models, err := listOpenAIModels(ctx, o.client)
	if err != nil {
		return nil
	}
	for _, name := range models {
		metadata := map[string]any{
			"model": map[string]any{
				"supports": map[string]any{
					"media":       true,
					"multiturn":   true,
					"systemRole":  true,
					"tools":       true,
					"toolChoice":  true,
					"constrained": true,
				},
			},
			"versions": []string{},
			"stage":    string(ai.ModelStageStable),
		}
		metadata["label"] = fmt.Sprintf("%s - %s", o.Provider, name)

		actions = append(actions, core.ActionDesc{
			Type:     core.ActionTypeModel,
			Name:     fmt.Sprintf("%s/%s", o.Provider, name),
			Key:      fmt.Sprintf("/%s/%s/%s", core.ActionTypeModel, o.Provider, name),
			Metadata: metadata,
		})
	}

	return actions
}

func (o *OpenAICompatible) ResolveAction(g *genkit.Genkit, atype core.ActionType, name string) error {
	switch atype {
	case core.ActionTypeModel:
		o.DefineModel(g, o.Provider, name, ai.ModelInfo{
			Label:    fmt.Sprintf("%s - %s", o.Provider, name),
			Stage:    ai.ModelStageStable,
			Versions: []string{},
			Supports: &Multimodal,
		})
	}

	return nil
}

func listOpenAIModels(ctx context.Context, client *openai.Client) ([]string, error) {
	models := []string{}
	iter := client.Models.ListAutoPaging(ctx)
	for iter.Next() {
		m := iter.Current()
		models = append(models, m.ID)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}

	return models, nil
}
