package gateway

import (
	"context"
	"errors"
	"fmt"

	"suno2api/internal/download"
	"suno2api/internal/generate"
	"suno2api/internal/session"
)

type nativeClient struct {
	generator  *generate.Client
	downloader *download.Client
	manager    *session.Manager
}

func defaultFactory(config AccountClientConfig) (AccountClient, error) {
	manager, err := session.NewManager(session.Config{TokenFile: config.TokenFile, AuthCookieFile: config.AuthCookieFile, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, err
	}
	provider := func(ctx context.Context) (string, error) {
		token, err := manager.Ensure(ctx)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrAuthentication), errors.Is(err, session.ErrRefreshCredentialsRequired), errors.Is(err, session.ErrUnsupportedCredential):
				return "", fmt.Errorf("%w: %w", generate.ErrAuthentication, err)
			case errors.Is(err, session.ErrRateLimited):
				return "", fmt.Errorf("%w: %w", generate.ErrRateLimited, err)
			default:
				return "", err
			}
		}
		return token, nil
	}
	generator, err := generate.NewClient(generate.Config{Protocol: config.Protocol, DefaultModel: config.Model, TokenFile: config.TokenFile, TokenProvider: provider, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, err
	}
	downloader := download.NewClient(download.Options{TokenFile: config.TokenFile, TokenProvider: provider, HTTPClient: config.HTTPClient})
	return &nativeClient{generator: generator, downloader: downloader, manager: manager}, nil
}

func (client *nativeClient) Generate(ctx context.Context, request generate.Request) (generate.Response, error) {
	return client.generator.Generate(ctx, request)
}
func (client *nativeClient) Get(ctx context.Context, ids []string) ([]generate.Clip, error) {
	return client.generator.Get(ctx, ids)
}
func (client *nativeClient) Billing(ctx context.Context) (generate.BillingInfo, error) {
	return client.generator.Billing(ctx)
}
func (client *nativeClient) Download(ctx context.Context, url, path string) (download.Result, error) {
	return client.downloader.Download(ctx, url, path)
}
func (client *nativeClient) Status(ctx context.Context) (session.Status, error) {
	return client.manager.Status(ctx)
}
func (client *nativeClient) Refresh(ctx context.Context) error {
	_, err := client.manager.Refresh(ctx)
	return err
}
func (client *nativeClient) Run(ctx context.Context, onEvent func(session.Event)) error {
	return client.manager.Run(ctx, onEvent)
}
