package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/TBXark/confstore"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/errgroup"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var BuildVersion = "dev"

type StdioMCPClientConfig struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
	Args    []string          `json:"args"`
}

type SSEMCPClientConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type MCPClientType string

const (
	MCPClientTypeStdio MCPClientType = "stdio"
	MCPClientTypeSSE   MCPClientType = "sse"
)

type MCPClientConfig struct {
	Type            MCPClientType   `json:"type"`
	Config          json.RawMessage `json:"config"`
	PanicIfInvalid  bool            `json:"panicIfInvalid"`
	LogEnabled      bool            `json:"logEnabled"`
	AuthTokens      []string        `json:"authTokens"`
	InstallCommands []string        `json:"installCommands"`
}

type SSEServerConfig struct {
	BaseURL          string   `json:"baseURL"`
	Addr             string   `json:"addr"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	GlobalAuthTokens []string `json:"globalAuthTokens"`
}

type Config struct {
	Server  SSEServerConfig            `json:"server"`
	Clients map[string]MCPClientConfig `json:"clients"`
}

func main() {
	conf := flag.String("config", "config.json", "path to config file or a http(s) url")
	version := flag.Bool("version", false, "print version and exit")
	help := flag.Bool("help", false, "print help and exit")
	flag.Parse()
	if *help {
		flag.Usage()
		return
	}
	if *version {
		fmt.Println(BuildVersion)
		return
	}
	config, err := confstore.Load[Config](*conf)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	start(config)
}

func start(config *Config) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var clientGroup errgroup.Group
	httpMux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:    config.Server.Addr,
		Handler: httpMux,
	}
	info := mcp.Implementation{
		Name:    config.Server.Name,
		Version: config.Server.Version,
	}

	go func() {
		log.Printf("Starting SSE server")
		log.Printf("SSE server listening on %s", config.Server.Addr)
		hErr := httpServer.ListenAndServe()
		if hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", hErr)
		}
	}()

	var installGroup errgroup.Group

	for name, clientConfig := range config.Clients {
		name := name
		clientConfig := clientConfig
		if len(clientConfig.InstallCommands) > 0 {
			for _, installCommand := range clientConfig.InstallCommands {
				installCommand := installCommand
				installGroup.Go(func() error {
					cmd := exec.CommandContext(ctx, "sh", "-c", installCommand)
					var stdout, stderr bytes.Buffer
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr

					log.Printf("<%s> Running install command: %s", name, installCommand)
					err := cmd.Run()
					if err != nil {
						log.Printf("<%s> Install command failed: %s, Error: %v, Stdout: %s, Stderr: %s", name, installCommand, err, stdout.String(), stderr.String())
					} else {
						log.Printf("<%s> Install command successful: %s, Stdout: %s, Stderr: %s", name, installCommand, stdout.String(), stderr.String())
					}
					return nil
				})
			}
		}
	}

	log.Println("Waiting for all installations to complete...")
	installGroup.Wait()
	log.Printf("All installations attempted. Please check logs for any installation failures.")

	for name, clientConfig := range config.Clients {
		name := name
		clientConfig := clientConfig
		clientGroup.Go(func() error {
			log.Printf("<%s> Creating client", name)
			mcpClient, err := newMCPClient(name, clientConfig)
			if err != nil {
				log.Printf("<%s> Failed to create MCP client: %v", name, err)
				if clientConfig.PanicIfInvalid {
					return fmt.Errorf("failed to create MCP client %s: %v", name, err)
				}
				return nil
			}

			serverOpts := []server.ServerOption{
				server.WithResourceCapabilities(true, true),
				server.WithRecovery(),
			}
			if clientConfig.LogEnabled {
				serverOpts = append(serverOpts, server.WithLogging())
			}
			mcpServer := server.NewMCPServer(
				config.Server.Name,
				config.Server.Version,
				serverOpts...,
			)
			sseServer := server.NewSSEServer(mcpServer,
				server.WithBaseURL(config.Server.BaseURL),
				server.WithBasePath(name),
			)

			log.Printf("<%s> Connecting", name)
			addErr := mcpClient.addToMCPServer(ctx, info, mcpServer)
			if addErr != nil {
				log.Printf("<%s> Failed to connect: %v", name, addErr)
				if clientConfig.PanicIfInvalid {
					return fmt.Errorf("failed to connect client %s: %v", name, addErr)
				}
				_ = mcpClient.Close()
				return nil
			}
			log.Printf("<%s> Connected", name)

			tokens := make([]string, 0, len(clientConfig.AuthTokens)+len(config.Server.GlobalAuthTokens))
			tokens = append(tokens, clientConfig.AuthTokens...)
			tokens = append(tokens, config.Server.GlobalAuthTokens...)
			httpMux.Handle(fmt.Sprintf("/%s/", name), chainMiddleware(sseServer, newAuthMiddleware(tokens)))

			httpServer.RegisterOnShutdown(func() {
				log.Printf("Closing client %s", name)
				_ = mcpClient.Close()
			})

			return nil
		})
	}

	log.Println("Waiting for all clients to connect...")
	err := clientGroup.Wait()
	if err != nil {
		log.Fatalf("Client initialization error: %v", err)
	}
	log.Println("All clients connection attempted.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
}

func parseMCPClientConfig(conf MCPClientConfig) (any, error) {
	switch conf.Type {
	case MCPClientTypeStdio:
		var config StdioMCPClientConfig
		err := json.Unmarshal(conf.Config, &config)
		if err != nil {
			return nil, err
		}
		return config, nil
	case MCPClientTypeSSE:
		var config SSEMCPClientConfig
		err := json.Unmarshal(conf.Config, &config)
		if err != nil {
			return nil, err
		}
		return config, nil
	default:
		return nil, errors.New("invalid client type")
	}
}

type Client struct {
	name     string
	needPing bool
	client   client.MCPClient
}

func newMCPClient(name string, conf MCPClientConfig) (*Client, error) {
	clientInfo, pErr := parseMCPClientConfig(conf)
	if pErr != nil {
		return nil, pErr
	}
	switch v := clientInfo.(type) {
	case StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		mcpClient, err := client.NewStdioMCPClient(v.Command, envs, v.Args...)
		if err != nil {
			if conf.PanicIfInvalid {
				return nil, fmt.Errorf("failed to create MCP client %s: %v", name, err)
			}
			return nil, err
		}
		return &Client{
			name:   name,
			client: mcpClient,
		}, nil

	case SSEMCPClientConfig:
		var options []transport.ClientOption
		if len(v.Headers) > 0 {
			options = append(options, client.WithHeaders(v.Headers))
		}
		mcpClient, err := client.NewSSEMCPClient(v.URL, options...)
		if err != nil {
			if conf.PanicIfInvalid {
				return nil, fmt.Errorf("failed to create MCP client %s: %v", name, err)
			}
			return nil, err
		}
		return &Client{
			name:     name,
			needPing: true,
			client:   mcpClient,
		}, nil
	}
	return nil, errors.New("invalid client type")
}

func (c *Client) addToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = clientInfo
	_, err := c.client.Initialize(ctx, initRequest)
	if err != nil {
		return err
	}
	log.Printf("<%s> Successfully initialized MCP client", c.name)

	err = c.addToolsToServer(ctx, mcpServer)
	if err != nil {
		return err
	}
	_ = c.addPromptsToServer(ctx, mcpServer)
	_ = c.addResourcesToServer(ctx, mcpServer)
	_ = c.addResourceTemplatesToServer(ctx, mcpServer)

	if c.needPing {
		go c.startPingTask(ctx)
	}
	return nil
}

func (c *Client) startPingTask(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("<%s> Context done, stopping ping", c.name)
			break
		case <-ticker.C:
			_ = c.client.Ping(ctx)
		}
	}
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	toolsRequest := mcp.ListToolsRequest{}
	for {
		tools, err := c.client.ListTools(ctx, toolsRequest)
		if err != nil {
			return err
		}
		if len(tools.Tools) == 0 {
			break
		}
		log.Printf("<%s> Successfully listed %d tools", c.name, len(tools.Tools))
		for _, tool := range tools.Tools {
			log.Printf("<%s> Adding tool %s", c.name, tool.Name)
			mcpServer.AddTool(tool, c.client.CallTool)
		}
		if tools.NextCursor == "" {
			break
		}
		toolsRequest.PaginatedRequest.Params.Cursor = tools.NextCursor
	}
	return nil
}

func (c *Client) addPromptsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	promptsRequest := mcp.ListPromptsRequest{}
	for {
		prompts, err := c.client.ListPrompts(ctx, promptsRequest)
		if err != nil {
			return err
		}
		if len(prompts.Prompts) == 0 {
			break
		}
		log.Printf("<%s> Successfully listed %d prompts", c.name, len(prompts.Prompts))
		for _, prompt := range prompts.Prompts {
			log.Printf("<%s> Adding prompt %s", c.name, prompt.Name)
			mcpServer.AddPrompt(prompt, c.client.GetPrompt)
		}
		if prompts.NextCursor == "" {
			break
		}
		promptsRequest.PaginatedRequest.Params.Cursor = prompts.NextCursor
	}
	return nil
}

func (c *Client) addResourcesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	resourcesRequest := mcp.ListResourcesRequest{}
	for {
		resources, err := c.client.ListResources(ctx, resourcesRequest)
		if err != nil {
			return err
		}
		if len(resources.Resources) == 0 {
			break
		}
		log.Printf("<%s> Successfully listed %d resources", c.name, len(resources.Resources))
		for _, resource := range resources.Resources {
			log.Printf("<%s> Adding resource %s", c.name, resource.Name)
			mcpServer.AddResource(resource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				readResource, e := c.client.ReadResource(ctx, request)
				if e != nil {
					return nil, e
				}
				return readResource.Contents, nil
			})
		}
		if resources.NextCursor == "" {
			break
		}
		resourcesRequest.PaginatedRequest.Params.Cursor = resources.NextCursor

	}
	return nil
}

func (c *Client) addResourceTemplatesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	resourceTemplatesRequest := mcp.ListResourceTemplatesRequest{}
	for {
		resourceTemplates, err := c.client.ListResourceTemplates(ctx, resourceTemplatesRequest)
		if err != nil {
			return err
		}
		if len(resourceTemplates.ResourceTemplates) == 0 {
			break
		}
		log.Printf("<%s> Successfully listed %d resource templates", c.name, len(resourceTemplates.ResourceTemplates))
		for _, resourceTemplate := range resourceTemplates.ResourceTemplates {
			log.Printf("<%s> Adding resource template %s", c.name, resourceTemplate.Name)
			mcpServer.AddResourceTemplate(resourceTemplate, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				readResource, e := c.client.ReadResource(ctx, request)
				if e != nil {
					return nil, e
				}
				return readResource.Contents, nil
			})
		}
		if resourceTemplates.NextCursor == "" {
			break
		}
		resourceTemplatesRequest.PaginatedRequest.Params.Cursor = resourceTemplates.NextCursor
	}
	return nil
}

func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				if strings.HasPrefix(token, "Bearer ") {
					token = strings.TrimPrefix(token, "Bearer ")
				}
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
