package codexsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/protocol"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/subprocess"
)

const (
	// defaultStreamCloseTimeout is the default timeout for waiting for result before closing stdin.
	defaultStreamCloseTimeout = 60 * time.Second
)

// getStreamCloseTimeout returns the stream close timeout from env var or default.
func getStreamCloseTimeout() time.Duration {
	if timeoutStr := os.Getenv("CODEX_STREAM_CLOSE_TIMEOUT"); timeoutStr != "" {
		if timeoutSec, err := strconv.Atoi(timeoutStr); err == nil && timeoutSec > 0 {
			return time.Duration(timeoutSec) * time.Second
		}
	}

	return defaultStreamCloseTimeout
}

// createStreamingTransport creates a transport for streaming mode.
func createStreamingTransport(
	log *slog.Logger,
	options *CodexAgentOptions,
) config.Transport {
	if options.Transport != nil {
		log.Debug("using injected custom transport for streaming")

		return options.Transport
	}

	log.Debug("creating app-server adapter transport for streaming mode")

	return subprocess.NewAppServerAdapter(log, options)
}

func buildInitialUserMessage(content UserMessageContent, options *CodexAgentOptions) map[string]any {
	message := map[string]any{
		"role":    "user",
		"content": mergeContentAndOptionImages(content, options.Images),
	}

	return map[string]any{
		"type":    "user",
		"message": message,
	}
}

func mergeContentAndOptionImages(content UserMessageContent, images []string) UserMessageContent {
	if len(images) == 0 {
		return content
	}

	blocks := append([]ContentBlock{}, content.Blocks()...)
	for _, path := range images {
		blocks = append(blocks, &InputLocalImageBlock{
			Type: BlockTypeLocalImage,
			Path: path,
		})
	}

	return NewUserMessageContentBlocks(blocks)
}

func prepareExecContent(
	content UserMessageContent,
	options *CodexAgentOptions,
) (string, []string, bool) {
	images := append([]string{}, options.Images...)

	if content.IsString() {
		return content.String(), images, true
	}

	parts := make([]string, 0, len(content.Blocks()))
	for _, block := range content.Blocks() {
		switch b := block.(type) {
		case *TextBlock:
			parts = append(parts, b.Text)
		case *InputMentionBlock:
			parts = append(parts, formatPathMention(b.Path))
		case *InputLocalImageBlock:
			images = append(images, b.Path)
		default:
			return "", nil, false
		}
	}

	return strings.Join(parts, "\n\n"), images, true
}

// getLoggerWithComponent returns a logger with the component field set.
func getLoggerWithComponent(options *CodexAgentOptions, component string) *slog.Logger {
	log := options.Logger
	if log == nil {
		log = NopLogger()
	}

	return log.With("component", component)
}

// validateAndConfigureOptions validates options and configures auto-settings.
// It returns an error if CanUseTool and PermissionPromptToolName are both set.
func validateAndConfigureOptions(options *CodexAgentOptions) error {
	return config.ConfigureToolPermissionPolicy(options)
}

// Query executes a one-shot query to the Codex CLI and returns an iterator of messages.
//
// By default, logging is disabled. Use WithLogger to enable logging:
//
//	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
//	for msg, err := range Query(ctx, Text("What is 2+2?"),
//	    WithLogger(logger),
//	    WithPermissionMode("acceptEdits"),
//	) {
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    // handle msg
//	}
//
// The iterator yields messages as they arrive from the Codex CLI, including assistant
// responses, tool use, and a final result message. Any errors during setup or
// execution are yielded inline with messages, allowing callers to handle all
// error conditions.
//
// Query supports hooks, CanUseTool callbacks, and SDK MCP servers through
// the protocol controller. When these options are configured, an initialization
// request is sent to the CLI before processing messages.
//
//nolint:gocyclo // query setup and streaming flow intentionally fan out by transport/backends.
func Query(
	ctx context.Context,
	content UserMessageContent,
	opts ...Option,
) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		options := applyAgentOptions(opts)

		if err := validateAndConfigureOptions(options); err != nil {
			yield(nil, err)

			return
		}

		log := options.Logger
		if log == nil {
			log = NopLogger()
		}

		log = log.With("component", "query")
		log.Debug("starting query execution")

		// Initialize OTel metrics recorder from providers if configured.
		initMetricsRecorder(options)

		var queryErr error

		var querySpan trace.Span

		ctx, querySpan = startQuerySpan(ctx, options, "query")

		defer func() {
			if queryErr != nil {
				querySpan.SetStatus(1, queryErr.Error()) // codes.Error = 1
			}

			querySpan.End()
		}()

		var transport config.Transport

		useAppServerQuery := false
		backend := config.QueryBackendExec
		execPrompt := ""
		execOptions := options

		if options.Transport != nil {
			transport = options.Transport

			log.Debug("using injected custom transport")
		} else {
			backend = config.SelectQueryBackend(options)
			if backend == config.QueryBackendExec {
				prompt, images, ok := prepareExecContent(content, options)
				if ok {
					execPrompt = prompt
					execOptionsCopy := *options
					execOptionsCopy.Images = images
					execOptions = &execOptionsCopy
				} else {
					backend = config.QueryBackendAppServer
				}
			}

			if err := config.ValidateOptionsForBackend(options, backend); err != nil {
				queryErr = err

				yield(nil, err)

				return
			}
		}

		if options.Transport == nil && backend == config.QueryBackendAppServer {
			useAppServerQuery = true

			log.Debug("creating app-server adapter transport for query mode")
			transport = subprocess.NewAppServerAdapter(log, options)
		} else if options.Transport == nil {
			log.Debug("creating CLI transport")

			transport = subprocess.NewCLITransport(log, execPrompt, execOptions)
		}

		log.Info("starting transport")

		if err := transport.Start(ctx); err != nil {
			log.Error("failed to start CLI", "error", err)

			queryErr = err

			yield(nil, err)

			return
		}

		defer func() {
			if err := transport.Close(); err != nil {
				log.Warn("failed to close transport", "error", err)
			}
		}()

		log.Info("successfully started Codex CLI")

		controller := protocol.NewController(log, transport)
		if err := controller.Start(ctx); err != nil {
			queryErr = fmt.Errorf("start protocol controller: %w", err)

			yield(nil, queryErr)

			return
		}

		defer controller.Stop()

		session := protocol.NewSession(log, controller, options)
		session.RegisterMCPServers()
		session.RegisterDynamicTools()
		session.RegisterHandlers()

		if useAppServerQuery || session.NeedsInitialization() {
			log.Debug("initializing session for hooks/callbacks")

			if err := session.Initialize(ctx); err != nil {
				queryErr = fmt.Errorf("initialize session: %w", err)

				yield(nil, queryErr)

				return
			}
		}

		if useAppServerQuery {
			userMessage := buildInitialUserMessage(content, options)

			data, err := json.Marshal(userMessage)
			if err != nil {
				queryErr = fmt.Errorf("marshal initial user message: %w", err)

				yield(nil, queryErr)

				return
			}

			if err := transport.SendMessage(ctx, data); err != nil {
				queryErr = fmt.Errorf("send initial user message: %w", err)

				yield(nil, queryErr)

				return
			}
		} else {
			log.Debug("closing stdin for one-shot query mode")

			if err := transport.EndInput(); err != nil {
				queryErr = fmt.Errorf("close stdin: %w", err)

				yield(nil, queryErr)

				return
			}
		}

		rawMessages := controller.Messages()

		var sessionID string

		log.Debug("reading messages from controller")

		for {
			select {
			case msg, ok := <-rawMessages:
				if !ok {
					log.Debug("raw message channel closed")

					if err := controller.FatalError(); err != nil {
						log.Error("error from transport", "error", err)

						queryErr = err

						yield(nil, err)
					}

					return
				}

				parsed, err := message.Parse(log, msg)
				if errors.Is(err, sdkerrors.ErrUnknownMessageType) {
					continue
				}

				if err != nil {
					log.Warn("failed to parse message", "error", err)

					queryErr = fmt.Errorf("parse message: %w", err)
					if !yield(nil, queryErr) {
						return
					}

					continue
				}

				if sys, ok := parsed.(*message.SystemMessage); ok && sys.Subtype == "thread.started" {
					if tid, ok := sys.Data["thread_id"].(string); ok && tid != "" {
						sessionID = tid
					}
				}

				if resultMsg, ok := parsed.(*message.ResultMessage); ok {
					if resultMsg.SessionID == "" && sessionID != "" {
						resultMsg.SessionID = sessionID
					}
				}

				if options.MetricsRecorder != nil {
					options.MetricsRecorder.Observe(ctx, parsed)
				}

				if !yield(parsed, nil) {
					log.Debug("yield returned false, stopping iteration")

					return
				}

				if _, isResult := parsed.(*message.ResultMessage); isResult {
					log.Debug("result message received, stopping query iteration")

					return
				}

			case <-controller.Done():
				log.Debug("controller stopped")

				if err := controller.FatalError(); err != nil {
					log.Error("error from transport", "error", err)
					queryErr = err

					yield(nil, err)
				}

				return

			case <-ctx.Done():
				log.Debug("context cancelled")

				queryErr = ctx.Err()

				yield(nil, ctx.Err())

				return
			}
		}
	}
}

// streamInputMessages streams messages to the transport's stdin.
// Returns error if sending fails, nil on successful completion.
func streamInputMessages(
	ctx context.Context,
	log *slog.Logger,
	transport config.Transport,
	messages iter.Seq[StreamingMessage],
	hasMCPOrHooks bool,
	resultReceived <-chan struct{},
	streamCloseTimeout time.Duration,
) (err error) {
	defer func() {
		if endErr := transport.EndInput(); endErr != nil {
			if err == nil {
				err = fmt.Errorf("end input: %w", endErr)
			}
		}
	}()

	for msg := range messages {
		select {
		case <-ctx.Done():
			log.Debug("context cancelled during message streaming")

			return ctx.Err()
		default:
		}

		data, marshalErr := json.Marshal(msg)
		if marshalErr != nil {
			log.Error("failed to marshal streaming message", "error", marshalErr)

			return fmt.Errorf("marshal streaming message: %w", marshalErr)
		}

		if sendErr := transport.SendMessage(ctx, data); sendErr != nil {
			log.Error("failed to send streaming message", "error", sendErr)

			return fmt.Errorf("send streaming message: %w", sendErr)
		}

		log.Debug("sent streaming message to CLI")
	}

	log.Debug("finished streaming all messages")

	if hasMCPOrHooks {
		log.Debug("waiting for result before closing stdin (MCP/hooks present)")

		select {
		case <-resultReceived:
			log.Debug("result received, proceeding to close stdin")
		case <-time.After(streamCloseTimeout):
			log.Warn("timeout waiting for result before closing stdin",
				slog.Duration("timeout", streamCloseTimeout))
		case <-ctx.Done():
			log.Debug("context cancelled while waiting for result")

			return ctx.Err()
		}
	}

	return nil
}

// QueryStream executes a streaming query with multiple input messages.
//
// The messages iterator yields StreamingMessage values that are sent to the
// Codex CLI via stdin in streaming mode.
//
// Example usage:
//
//	messages := codexsdk.MessagesFromSlice([]codexsdk.StreamingMessage{
//	    codexsdk.NewUserMessage(codexsdk.Text("Hello")),
//	    codexsdk.NewUserMessage(codexsdk.Text("How are you?")),
//	})
//
//	for msg, err := range codexsdk.QueryStream(ctx, messages,
//	    codexsdk.WithPermissionMode("acceptEdits"),
//	) {
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    // Handle messages
//	}
func QueryStream(
	ctx context.Context,
	messages iter.Seq[StreamingMessage],
	opts ...Option,
) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		options := applyAgentOptions(opts)

		if err := validateAndConfigureOptions(options); err != nil {
			yield(nil, err)

			return
		}

		log := getLoggerWithComponent(options, "query_stream")
		log.Debug("starting streaming query execution")

		// Initialize OTel metrics recorder from providers if configured.
		initMetricsRecorder(options)

		var queryErr error

		var querySpan trace.Span

		ctx, querySpan = startQuerySpan(ctx, options, "query_stream")

		defer func() {
			if queryErr != nil {
				querySpan.SetStatus(1, queryErr.Error()) // codes.Error = 1
			}

			querySpan.End()
		}()

		// QueryStream uses app-server semantics unless a custom transport is injected.
		if options.Transport == nil {
			if err := config.ValidateOptionsForBackend(options, config.QueryBackendAppServer); err != nil {
				queryErr = err

				yield(nil, err)

				return
			}
		}

		transport := createStreamingTransport(log, options)

		log.Info("starting transport in streaming mode")

		if err := transport.Start(ctx); err != nil {
			log.Error("failed to start CLI", "error", err)

			queryErr = err

			yield(nil, err)

			return
		}

		defer func() {
			if err := transport.Close(); err != nil {
				log.Warn("failed to close transport", "error", err)
			}
		}()

		log.Info("successfully started Codex CLI in streaming mode")

		controller := protocol.NewController(log, transport)
		if err := controller.Start(ctx); err != nil {
			queryErr = fmt.Errorf("start protocol controller: %w", err)

			yield(nil, queryErr)

			return
		}

		defer controller.Stop()

		session := protocol.NewSession(log, controller, options)
		session.RegisterMCPServers()
		session.RegisterDynamicTools()
		session.RegisterHandlers()

		log.Debug("initializing session for streaming mode")

		if err := session.Initialize(ctx); err != nil {
			queryErr = fmt.Errorf("initialize session: %w", err)

			yield(nil, queryErr)

			return
		}

		rawMessages := controller.Messages()

		hasMCPOrHooks := len(options.MCPServers) > 0 || len(options.Hooks) > 0 || len(options.SDKTools) > 0

		var resultReceived chan struct{}

		if hasMCPOrHooks {
			resultReceived = make(chan struct{})
		}

		var closeResultOnce sync.Once

		closeResult := func() {
			if resultReceived != nil {
				closeResultOnce.Do(func() {
					close(resultReceived)
				})
			}
		}

		streamCloseTimeout := getStreamCloseTimeout()

		g, gCtx := errgroup.WithContext(ctx)

		g.Go(func() error {
			return streamInputMessages(
				gCtx,
				log,
				transport,
				messages,
				hasMCPOrHooks,
				resultReceived,
				streamCloseTimeout,
			)
		})

		defer func() { _ = g.Wait() }()

		defer func() {
			if hasMCPOrHooks {
				closeResult()
			}
		}()

		log.Debug("reading messages from controller")

		queryErr = streamReceiveLoop(ctx, log, controller, options,
			rawMessages, gCtx, g, closeResult, hasMCPOrHooks, yield)
	}
}

// streamReceiveLoop reads parsed messages from the controller and yields them.
// It returns the first fatal error encountered, or nil on clean completion.
func streamReceiveLoop(
	ctx context.Context,
	log *slog.Logger,
	controller *protocol.Controller,
	options *CodexAgentOptions,
	rawMessages <-chan map[string]any,
	gCtx context.Context,
	g *errgroup.Group,
	closeResult func(),
	hasMCPOrHooks bool,
	yield func(Message, error) bool,
) error {
	for {
		select {
		case msg, ok := <-rawMessages:
			if !ok {
				log.Debug("raw message channel closed")

				if err := controller.FatalError(); err != nil {
					log.Error("error from transport", "error", err)
					yield(nil, err)

					return err
				}

				return nil
			}

			parsed, err := message.Parse(log, msg)
			if errors.Is(err, sdkerrors.ErrUnknownMessageType) {
				continue
			}

			if err != nil {
				log.Warn("failed to parse message", "error", err)

				fmtErr := fmt.Errorf("parse message: %w", err)
				if !yield(nil, fmtErr) {
					return fmtErr
				}

				continue
			}

			if hasMCPOrHooks {
				if _, isResult := parsed.(*message.ResultMessage); isResult {
					closeResult()
				}
			}

			if options.MetricsRecorder != nil {
				options.MetricsRecorder.Observe(ctx, parsed)
			}

			if !yield(parsed, nil) {
				log.Debug("yield returned false, stopping iteration")

				return nil
			}

			if _, isResult := parsed.(*message.ResultMessage); isResult {
				log.Debug("result message received, stopping iteration")

				return nil
			}

		case <-controller.Done():
			log.Debug("controller stopped")

			if err := controller.FatalError(); err != nil {
				log.Error("error from transport", "error", err)
				yield(nil, err)

				return err
			}

			return nil

		case <-ctx.Done():
			log.Debug("context cancelled")
			yield(nil, ctx.Err())

			return ctx.Err()

		case <-gCtx.Done():
			if err := g.Wait(); err != nil {
				log.Error("streaming goroutine failed", "error", err)
				yield(nil, err)
			}

			return nil
		}
	}
}
