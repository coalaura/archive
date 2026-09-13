package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/coalaura/archive/internal/archiver"
	"github.com/coalaura/archive/internal/config"
	gitprovider "github.com/coalaura/archive/internal/provider/git"
	"github.com/coalaura/archive/internal/provider/huggingface"
	"github.com/coalaura/plain/minimal"
	"github.com/urfave/cli/v3"
)

const (
	gitDefaultRevision         = "HEAD"
	huggingFaceDefaultRevision = "main"
)

type application struct {
	workingDirectory string
	config           config.Config
	logger           *minimal.Minimal
}

func (application *application) command() *cli.Command {
	return &cli.Command{
		Name:  "archive",
		Usage: "stream remote artifacts directly into compressed archives",
		Commands: []*cli.Command{
			application.gitCommand(),
			application.huggingFaceCommand(),
		},
	}
}

func (application *application) gitCommand() *cli.Command {
	return &cli.Command{
		Name:      "git",
		Usage:     "archive a Git repository",
		ArgsUsage: "<repository>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "revision",
				Aliases: []string{"r"},
				Value:   gitDefaultRevision,
				Usage:   "branch, tag, or commit to archive",
			},
		},
		Action: application.archiveGit,
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
				Value:   huggingFaceDefaultRevision,
				Usage:   "branch, tag, or full commit SHA to archive",
			},
		},
		Action: application.archiveHuggingFace,
	}
}

func (application *application) archiveGit(ctx context.Context, command *cli.Command) (resultErr error) {
	if command.NArg() != 1 {
		return errors.New("usage: archive git <repository>")
	}

	reference := strings.TrimSpace(command.Args().First())
	if reference == "" {
		return errors.New("repository cannot be empty")
	}

	revision := strings.TrimSpace(command.String("revision"))
	if revision == "" {
		return errors.New("revision cannot be empty")
	}

	outputDirectory := filepath.Join(application.workingDirectory, "archive", "git")
	source := gitprovider.New(outputDirectory)

	defer func() {
		closeErr := source.Close()
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary Git repository: %w", closeErr))
		}
	}()

	archiveWriter := archiver.New(application.logger)

	_, resultErr = archiveWriter.Archive(ctx, source, reference, revision, outputDirectory)

	return resultErr
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
	logger := minimal.New()

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
