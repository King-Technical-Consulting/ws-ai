package openaicompat

import (
	"context"
	"fmt"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/jking323/ws/internal/gateway"
)

// Embed implements gateway.Embedder with POST /embeddings. llama-server,
// vLLM, Ollama and OpenAI all serve it with the same shape.
func (a *Adapter) Embed(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error) {
	params := sdk.EmbeddingNewParams{
		Input: sdk.EmbeddingNewParamsInputUnion{OfArrayOfStrings: req.Inputs},
		Model: sdk.EmbeddingModel(ep.ModelName),
	}
	if req.Dimensions > 0 {
		params.Dimensions = param.NewOpt(int64(req.Dimensions))
	}
	var reqOpts []option.RequestOption
	for k, v := range ep.ExtraBody {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}
	client := a.client(p)
	res, err := client.Embeddings.New(ctx, params, reqOpts...)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: embed: %w", err)
	}
	if len(res.Data) != len(req.Inputs) {
		return nil, fmt.Errorf("openaicompat: embed: %d vectors for %d inputs", len(res.Data), len(req.Inputs))
	}
	out := &gateway.EmbedResponse{Model: res.Model, Vectors: make([][]float32, len(req.Inputs)), Usage: gateway.Usage{InputTokens: int(res.Usage.PromptTokens)}}
	for _, d := range res.Data {
		if d.Index < 0 || int(d.Index) >= len(out.Vectors) {
			return nil, fmt.Errorf("openaicompat: embed: index %d out of range", d.Index)
		}
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		out.Vectors[d.Index] = v
	}
	return out, nil
}
