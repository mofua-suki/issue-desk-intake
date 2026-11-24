package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"issue-desk-intake/server/internal/admincli"
	"issue-desk-intake/server/internal/startup"
)

var revision = "development"

func main() {
	if err := run(); err != nil {
		slog.Error("Serverの操作に失敗しました", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		return admincli.Run(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	}
	flags := flag.NewFlagSet("intake", flag.ContinueOnError)
	configFile := flags.String("config", "config.local.json", "Serverの設定ファイル")
	application := flags.String("application-dir", ".", "配布されたアプリの場所")
	declared := flags.String("settings", "", "宣言設定（省略時はdeploy/nas/intake-settings.json）")
	assets := flags.String("assets", "", "Clientの静的ファイル（省略時はclient/dist）")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不要な位置引数があります")
	}
	root, err := filepath.Abs(*application)
	if err != nil {
		return errors.New("アプリの場所を解決できません")
	}
	if *declared == "" {
		*declared = filepath.Join(root, "deploy", "nas", "intake-settings.json")
	}
	if *assets == "" {
		*assets = filepath.Join(root, "client", "dist")
	}
	config, err := startup.Load(*configFile, root, *declared)
	if err != nil {
		return err
	}
	input := make(chan os.Signal, 4)
	signal.Notify(input, syscall.SIGUSR2, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(input)
	notifications := newSignalNotifications(context.Background(), input)
	defer notifications.Close()
	return startup.Run(notifications.Context, startup.RunOptions{Handoff: notifications.Handoff, Config: config, DSN: os.Getenv("ISSUE_DESK_DATABASE_URL"), PeerURL: os.Getenv("ISSUE_DESK_PEER_URL"), Assets: *assets, Revision: revision, Log: func(message string) { slog.Info(message) }})
}
