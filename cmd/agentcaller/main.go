// Command agentcaller provides headless WhatsApp calling operations.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
)

var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, err)
		}
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errUsage
	}
	logger, err := commandLogger(stderr)
	if err != nil {
		return err
	}
	ctx = logger.WithContext(ctx)
	switch args[0] {
	case "pair":
		cfg, err := parsePairArgs(args[1:], stderr)
		if err != nil {
			return err
		}
		return runPair(ctx, cfg, stderr)
	case "notify":
		cfg, err := parseNotifyArgs(args[1:], stderr)
		if err != nil {
			return err
		}
		return runNotify(ctx, cfg, stderr)
	case "call":
		cfg, err := parseCallArgs(args[1:], stderr)
		if err != nil {
			return err
		}
		return runCall(ctx, cfg, stderr)
	case "talk":
		cfg, err := parseTalkArgs(args[1:], stderr)
		if err != nil {
			return err
		}
		return runTalk(ctx, cfg, stderr)
	default:
		printUsage(stderr)
		return errUsage
	}
}

func commandLogger(out io.Writer) (zerolog.Logger, error) {
	level := zerolog.WarnLevel
	name := "AGENTCALLER_LOG_LEVEL"
	configured := os.Getenv(name)
	if configured == "" { // the name the command had as meowcaller
		name = "MEOW_LOG_LEVEL"
		configured = os.Getenv(name)
	}
	if configured != "" {
		parsed, err := zerolog.ParseLevel(configured)
		if err != nil {
			return zerolog.Logger{}, fmt.Errorf("invalid %s: %w", name, err)
		}
		level = parsed
	}
	return zerolog.New(out).Level(level).With().Timestamp().Logger(), nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  agentcaller pair [flags]")
	fmt.Fprintln(w, "  agentcaller notify [flags] <target> <audio.mp3|wav|ogg|opus|mulaw|ulaw|->")
	fmt.Fprintln(w, "  agentcaller call [flags] <target>           (two-way call: PCM in, PCM out, JSON events)")
	fmt.Fprintln(w, "  agentcaller call -answer [flags] <caller|any>")
	fmt.Fprintln(w, "  agentcaller talk [flags] <target>           (live VAD→STT→LLM→TTS call)")
	fmt.Fprintln(w, "  agentcaller talk -dry-run [flags] <input.wav>")
	fmt.Fprintln(w, "  agentcaller talk -replay [flags] -replay <input.wav> [--listen-only] (VAD-Replay)")
}
