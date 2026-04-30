// Package bedrock adapts Amazon Bedrock Runtime's ConverseStream API to
// Starling's Provider interface.
package bedrock

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/jerkeyray/starling/provider"
)

const defaultAPIVersion = bedrockruntime.ServiceAPIVersion

// New constructs a Provider that talks to Amazon Bedrock Runtime.
//
// If WithAWSConfig is not supplied, New loads AWS's default configuration
// chain. Set WithRegion when the environment does not already provide one.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		providerID: "bedrock",
		apiVersion: defaultAPIVersion,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.client == nil {
		awsCfg := cfg.awsConfig
		if !cfg.awsConfigSet {
			loadOpts := make([]func(*awsconfig.LoadOptions) error, 0, 1)
			if cfg.region != "" {
				loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.region))
			}
			loaded, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
			if err != nil {
				return nil, fmt.Errorf("bedrock: load AWS config: %w", err)
			}
			awsCfg = loaded
		}
		if cfg.region != "" {
			awsCfg.Region = cfg.region
		}

		client := bedrockruntime.NewFromConfig(awsCfg, func(o *bedrockruntime.Options) {
			if cfg.httpClient != nil {
				o.HTTPClient = cfg.httpClient
			}
			if cfg.baseEndpoint != "" {
				o.BaseEndpoint = aws.String(cfg.baseEndpoint)
			}
		})
		cfg.client = awsConverseClient{client: client}
	}

	return &bedrockProvider{cfg: cfg}, nil
}

// Option configures the Bedrock provider.
type Option func(*config)

type config struct {
	awsConfig    aws.Config
	awsConfigSet bool
	region       string
	baseEndpoint string
	httpClient   bedrockruntime.HTTPClient
	providerID   string
	apiVersion   string
	client       converseClient
}

// WithAWSConfig supplies a fully-loaded AWS SDK config.
func WithAWSConfig(cfg aws.Config) Option {
	return func(c *config) {
		c.awsConfig = cfg
		c.awsConfigSet = true
	}
}

// WithRegion sets the AWS region used by the Bedrock client.
func WithRegion(region string) Option { return func(c *config) { c.region = region } }

// WithBaseEndpoint overrides the Bedrock Runtime base endpoint.
func WithBaseEndpoint(endpoint string) Option {
	return func(c *config) { c.baseEndpoint = endpoint }
}

// WithHTTPClient supplies a custom HTTP client.
func WithHTTPClient(client bedrockruntime.HTTPClient) Option {
	return func(c *config) { c.httpClient = client }
}

// WithProviderID overrides Info().ID. Defaults to "bedrock".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// WithAPIVersion overrides Info().APIVersion.
func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type bedrockProvider struct {
	cfg config
}

func (p *bedrockProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
}

func (p *bedrockProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Tools:         true,
		ToolChoice:    true,
		Reasoning:     true,
		StopSequences: true,
		RequestID:     true,
	}
}

func (p *bedrockProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if req == nil {
		return nil, errors.New("bedrock: nil Request")
	}
	input, err := buildInput(req)
	if err != nil {
		return nil, err
	}
	out, err := p.cfg.client.ConverseStream(ctx, input)
	if err != nil {
		return nil, err
	}
	if out.stream == nil {
		return nil, errors.New("bedrock: ConverseStream returned nil stream")
	}
	return newBedrockStream(out.stream, out.requestID), nil
}

type converseOutput struct {
	stream    *bedrockruntime.ConverseStreamEventStream
	requestID string
}

type converseClient interface {
	ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput) (*converseOutput, error)
}

type awsConverseClient struct {
	client *bedrockruntime.Client
}

func (c awsConverseClient) ConverseStream(ctx context.Context, input *bedrockruntime.ConverseStreamInput) (*converseOutput, error) {
	out, err := c.client.ConverseStream(ctx, input)
	if err != nil {
		return nil, err
	}
	reqID, _ := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata)
	return &converseOutput{stream: out.GetStream(), requestID: reqID}, nil
}
