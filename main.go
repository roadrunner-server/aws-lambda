package main

import (
	_ "embed"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/roadrunner-server/config/v5"
	"github.com/roadrunner-server/endure/v2"
	"github.com/roadrunner-server/logger/v5"
	"github.com/roadrunner-server/server/v5"
)

//go:embed .rr.yaml
var rrYaml []byte

func main() {
	configureEnvironment()

	cont := endure.New(slog.LevelError)

	cfg := &config.Plugin{
		Version:   "2023.3.0",
		Timeout:   time.Second * 30,
		Type:      "yaml",
		ReadInCfg: rrYaml,
	}

	err := cont.RegisterAll(
		cfg,
		&logger.Plugin{},
		&Plugin{},
		&server.Plugin{},
	)
	if err != nil {
		log.Fatal(err)
	}

	err = cont.Init()
	if err != nil {
		log.Fatal(err)
	}

	ch, err := cont.Serve()
	if err != nil {
		log.Fatal(err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	wg := &sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			select {
			case e := <-ch:
				err = cont.Stop()
				if err != nil {
					log.Println(e.Error.Error())
				}
			case <-sig:
				err = cont.Stop()
				if err != nil {
					log.Println(err.Error())
				}
				return
			}
		}
	}()

	wg.Wait()
}

func configureEnvironment() {
	_ = os.Setenv("PATH", os.Getenv("PATH")+":"+os.Getenv("LAMBDA_TASK_ROOT"))
	_ = os.Setenv("LD_LIBRARY_PATH", "./lib:/lib64:/usr/lib64")
}
