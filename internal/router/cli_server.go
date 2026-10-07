package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Serving a request by running a local command instead of calling an HTTP
// endpoint.
//
// The command runs under the operator's own login and owns its own credential
// store; the router only reads the text it prints. That is what makes this shape
// legitimate: Anthropic's policy forbids a proxy from collecting or
// intermediating Claude.ai credentials, and permits an end user running the
// unmodified Claude Code binary with their own subscription. Nothing here reads
// a token, a credential file, or a keychain entry — and the config refuses to
// accept an API key on such an upstream, because there is nothing to give it.

// attemptCLI serves one request through the upstream's command. It returns no
// *http.Response, because there is no upstream response to own: the reply is
// written here rather than by the shared HTTP path.
func (s *Server) attemptCLI(ctx context.Context, w http.ResponseWriter, plan *requestPlan, c candidate, logger *slog.Logger) (*http.Response, attemptOutcome, error) {
	if c.upstream.cfg.CLIModeValue() == CLIPersistent {
		return s.attemptPersistentCLI(ctx, w, plan, c, logger)
	}
	timeout := c.upstream.cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = s.cfg.Defaults.MaxStreamDuration.Duration()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt, err := newCLIPrompt(plan.fields)
	if err != nil {
		return nil, attemptOutcome{}, fmt.Errorf("upstream %s: %w", c.upstream.name, err)
	}
	argv, err := buildCLICommand(c.upstream.cfg.Command, prompt)
	if err != nil {
		return nil, attemptOutcome{}, fmt.Errorf("upstream %s: %w", c.upstream.name, err)
	}

	outcome := attemptOutcome{status: http.StatusOK}
	if plan.stream {
		if serr := s.streamCLI(ctx, w, plan, c, argv, &outcome, logger); serr != nil {
			return nil, attemptOutcome{}, serr
		}
		return nil, outcome, nil
	}

	run, err := runCLICommand(ctx, argv, nil)
	if err != nil {
		return nil, attemptOutcome{}, err
	}
	recordCLIUsage(&outcome, run)

	// The command yields text; it is wrapped in whichever wire the client
	// speaks, so no translation step is involved.
	body := cliOpenAIResponse(run, plan.alias)
	if plan.api == APIAnthropicMessages {
		body = cliAnthropicResponse(run, plan.alias)
	}

	// There is no upstream response to copy headers from, but the body is built
	// in full first, so Content-Length stays honest.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Router-Upstream", c.upstream.name)
	w.Header().Set("X-Router-Upstream-Model", c.target.Model)
	w.WriteHeader(http.StatusOK)
	outcome.bytes = int64(len(body))
	if _, werr := w.Write(body); werr != nil {
		logger.Warn("client write failed", "upstream", c.upstream.name, "error", werr.Error())
	}
	return nil, outcome, nil
}

// streamCLI relays a CLI run as Server-Sent Events. The run is rendered into
// OpenAI chunks, which are converted when the client speaks a different wire.
func (s *Server) streamCLI(ctx context.Context, w http.ResponseWriter, plan *requestPlan, c candidate, argv []string, outcome *attemptOutcome, logger *slog.Logger) error {
	flusher, _ := w.(http.Flusher)
	xlate := newStreamTranslator(plan.api, APIOpenAICompletions, plan.alias)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Router-Upstream", c.upstream.name)
	w.Header().Set("X-Router-Upstream-Model", c.target.Model)
	w.WriteHeader(http.StatusOK)

	// raw writes exactly what the client will see; emit passes bytes through the
	// translator first. Keeping them apart means the translator's own trailing
	// output is not fed back into it.
	raw := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		outcome.bytes += int64(len(b))
		return nil
	}
	emit := func(b []byte) error {
		if xlate == nil {
			return raw(b)
		}
		return raw(xlate.Write(b))
	}

	chunks := newCLIOpenAIStream(plan.alias)
	if err := emit(chunks.Start()); err != nil {
		return err
	}

	run, err := runCLICommand(ctx, argv, func(delta string) error { return emit(chunks.Text(delta)) })
	if err != nil {
		// Bytes may already have reached the client, so this cannot fail over.
		// The stream is terminated cleanly — a hanging client is worse than a
		// finished one — and the outcome records the failure so the log shows
		// it rather than silently reporting a success.
		logger.Error("cli upstream failed mid-stream",
			"upstream", c.upstream.name, "error", err.Error())
		outcome.status = http.StatusBadGateway
		recordCLIUsage(outcome, cliRun{})
		if werr := emit(chunks.Done(cliRun{})); werr != nil {
			return werr
		}
	} else {
		recordCLIUsage(outcome, run)
		if werr := emit(chunks.Done(run)); werr != nil {
			return werr
		}
	}

	if xlate != nil {
		if werr := raw(xlate.Close()); werr != nil {
			return werr
		}
	}
	return nil
}

// recordCLIUsage copies what the command reported into the outcome. A cost the
// command states is carried through rather than recomputed: the agent knows what
// it spent, and the router would only be guessing.
func recordCLIUsage(outcome *attemptOutcome, run cliRun) {
	outcome.usage = run.Usage
	outcome.hasUsage = run.Usage != (Usage{})
	if run.HasCost {
		outcome.costUSD = run.CostUSD
		outcome.hasCost = true
	}
}

func (s *Server) attemptPersistentCLI(ctx context.Context, w http.ResponseWriter, plan *requestPlan, c candidate, logger *slog.Logger) (*http.Response, attemptOutcome, error) {
	if c.upstream.sessions == nil {
		return nil, attemptOutcome{}, errors.New("persistent CLI upstream has no session manager")
	}
	timeout := c.upstream.cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = s.cfg.Defaults.MaxStreamDuration.Duration()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var firstPrompt, nextPrompt string
	var err error
	if c.upstream.cfg.ToolMode == CLIToolsClient {
		firstPrompt, err = newCLIClientToolPrompt(plan.fields)
		nextPrompt, err = newCLIContinuationPrompt(plan.fields)
	} else {
		firstPrompt, err = newCLIPrompt(plan.fields)
		nextPrompt, err = newCLILastPrompt(plan.fields)
	}
	if err != nil {
		return nil, attemptOutcome{}, fmt.Errorf("upstream %s: %w", c.upstream.name, err)
	}
	sessionKey := plan.alias + "\x00" + plan.sessionKey

	outcome := attemptOutcome{status: http.StatusOK}
	if plan.stream {
		flusher, _ := w.(http.Flusher)
		stream := newCLIOpenAIStream(plan.alias)
		xlate := newStreamTranslator(plan.api, APIOpenAICompletions, plan.alias)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Router-Upstream", c.upstream.name)
		w.Header().Set("X-Router-Upstream-Model", c.target.Model)
		w.WriteHeader(http.StatusOK)
		raw := func(b []byte) error {
			if len(b) == 0 {
				return nil
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			outcome.bytes += int64(len(b))
			return nil
		}
		emit := func(b []byte) error {
			if xlate == nil {
				return raw(b)
			}
			return raw(xlate.Write(b))
		}
		if err := emit(stream.Start()); err != nil {
			return nil, attemptOutcome{}, err
		}
		run, runErr := c.upstream.sessions.Run(ctx, sessionKey, c.target.Model, firstPrompt, nextPrompt,
			func(delta string) error { return emit(stream.Text(delta)) })
		if runErr != nil {
			outcome.status = http.StatusBadGateway
			logger.Error("persistent cli upstream failed mid-stream",
				"upstream", c.upstream.name, "error", runErr.Error())
			_ = emit(stream.Done(cliRun{}))
		} else {
			recordCLIUsage(&outcome, run)
			_ = emit(stream.Done(run))
		}
		if xlate != nil {
			_ = raw(xlate.Close())
		}
		return nil, outcome, nil
	}

	run, err := c.upstream.sessions.Run(ctx, sessionKey, c.target.Model, firstPrompt, nextPrompt, nil)
	if err != nil {
		return nil, attemptOutcome{}, err
	}
	recordCLIUsage(&outcome, run)
	body := cliOpenAIResponse(run, plan.alias)
	if plan.api == APIAnthropicMessages {
		body = cliAnthropicResponse(run, plan.alias)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Router-Upstream", c.upstream.name)
	w.Header().Set("X-Router-Upstream-Model", c.target.Model)
	w.WriteHeader(http.StatusOK)
	outcome.bytes = int64(len(body))
	if _, err := w.Write(body); err != nil {
		logger.Warn("client write failed", "upstream", c.upstream.name, "error", err.Error())
	}
	return nil, outcome, nil
}
