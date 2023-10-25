// Copyright 2020 gorse Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/zhenghaoz/gorse/base/log"
	"github.com/zhenghaoz/gorse/cmd/version"
	"github.com/zhenghaoz/gorse/server"
	"go.uber.org/zap"
	_ "net/http/pprof"
	"os"
	"os/signal"
)

var serverCommand = &cobra.Command{
	Use:   "gorse-server",
	Short: "The server node of gorse recommender system.",
	Run: func(cmd *cobra.Command, args []string) {
		// show version
		showVersion, _ := cmd.PersistentFlags().GetBool("version")
		if showVersion {
			fmt.Println(version.BuildInfo())
			return
		}

		// setup logger
		debug, _ := cmd.PersistentFlags().GetBool("debug")
		log.SetLogger(cmd.PersistentFlags(), debug)

		// create server
		masterPort, _ := cmd.PersistentFlags().GetInt("master-port")
		masterHost, _ := cmd.PersistentFlags().GetString("master-host")
		httpPort, _ := cmd.PersistentFlags().GetInt("http-port")
		httpHost, _ := cmd.PersistentFlags().GetString("http-host")
		cachePath, _ := cmd.PersistentFlags().GetString("cache-path")
		s := server.NewServer(masterHost, masterPort, httpHost, httpPort, cachePath)

		// stop server
		done := make(chan struct{})
		go func() {
			sigint := make(chan os.Signal, 1)
			signal.Notify(sigint, os.Interrupt)
			<-sigint
			s.Shutdown()
			close(done)
		}()

		// start server
		s.Serve()
		<-done
		log.Logger().Info("stop gorse server successfully")
	},
}

func init() {
	log.AddFlags(serverCommand.PersistentFlags())
	serverCommand.PersistentFlags().BoolP("version", "v", false, "gorse version")
	serverCommand.PersistentFlags().Int("master-port", 8086, "port of master node")
	serverCommand.PersistentFlags().String("master-host", "127.0.0.1", "host of master node")
	serverCommand.PersistentFlags().Int("http-port", 8087, "host for RESTful APIs and Prometheus metrics export")
	serverCommand.PersistentFlags().String("http-host", "127.0.0.1", "port for RESTful APIs and Prometheus metrics export")
	serverCommand.PersistentFlags().Bool("debug", false, "use debug log mode")
	serverCommand.PersistentFlags().String("cache-path", "server_cache.data", "path of cache file")
}

func main() {
	if err := serverCommand.Execute(); err != nil {
		log.Logger().Fatal("failed to execute", zap.Error(err))
	}
}
