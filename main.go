package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/coalaura/archive/internal/archiver"
	"github.com/coalaura/archive/internal/config"
	"github.com/coalaura/archive/internal/provider/huggingface"
	"github.com/coalaura/plain"
	"github.com/urfave/cli/v3"
)

const defaultRevision = "main"

type application struct {
	workingDirectory string
	config           config.Config
	logger           *plain.Plain
}

func (application *application) command() *cli.Command {
	return &cli.Command{
		Name:  "archive",
		Usage: "stream remote artifacts directly into compressed archives",
		Commands: []*cli.Command{
			application.huggingFaceCommand(),
		},
	}
}

func (application *application) huggingFaceCommand() *cli.Command {
	return &cli.Command{
		Name:      "huggingface",
		Aliases:   []string{"hf"},
		Usage:     "archive a Hugging Face model repository",
		ArgsUsage: "<repository>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "revision",
				Aliases: []string{"r"},
				Value:   defaultRevision,
				Usage:   "branch, tag, or full commit SHA to archive",
			},
		},
		Action: application.archiveHuggingFace,
	}
}

func (application *application) archiveHuggingFace(ctx context.Context, command *cli.Command) error {
	if command.NArg() != 1 {
		return errors.New("usage: archive huggingface <repository>")
	}

	reference := strings.TrimSpace(command.Args().First())
	if reference == "" {
		return errors.New("repository cannot be empty")
	}

	revision := strings.TrimSpace(command.String("revision"))
	if revision == "" {
		return errors.New("revision cannot be empty")
	}

	source := huggingface.New(application.config.Tokens.HuggingFace)
	outputDirectory := filepath.Join(application.workingDirectory, "archive", source.Name())
	archiveWriter := archiver.New(application.logger)

	_, err := archiveWriter.Archive(ctx, source, reference, revision, outputDirectory)

	return err
}

func main() {
	logger := plain.New()

	workingDirectory, err := os.Getwd()
	if err != nil {
		logger.Errorf("Get working directory: %v\n", err)
		os.Exit(1)
	}

	appConfig, err := config.Load(filepath.Join(workingDirectory, "config.yml"))
	if err != nil {
		logger.Errorln(err)
		os.Exit(1)
	}

	application := &application{
		workingDirectory: workingDirectory,
		config:           appConfig,
		logger:           logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err = application.command().Run(ctx, os.Args)
	if err != nil {
		logger.Errorln(err)
		os.Exit(1)
	}
}
