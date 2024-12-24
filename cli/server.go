// SPDX-License-Identifier: MPL-2.0
// tomphttp/bare-server-go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tomphttp/bare-server-go"
)

func main() {
	var directory string
	var host string
	var port int
	var errors bool
	var localAddress string
	var family int
	var maintainer string
	var maintainerFile string

	var rootCmd = &cobra.Command{
		Use:     "bare-server",
		Short:   "Bare server implementation in Go",
		Version: "0.1.0",
		Run: func(cmd *cobra.Command, args []string) {
			if maintainer != "" && maintainerFile != "" {
				fmt.Fprintln(os.Stderr, "Error: Specify either -m or -mf, not both.")
				os.Exit(1)
			}

			var maintainerData *bare.BareMaintainer
			if maintainer != "" {
				if err := json.Unmarshal([]byte(maintainer), &maintainerData); err != nil {
					fmt.Fprintf(os.Stderr, "Error parsing maintainer data: %s\n", err)
					os.Exit(1)
				}
			} else if maintainerFile != "" {
				data, err := os.ReadFile(maintainerFile)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading maintainer file: %s\n", err)
					os.Exit(1)
				}
				if err := json.Unmarshal(data, &maintainerData); err != nil {
					fmt.Fprintf(os.Stderr, "Error parsing maintainer data: %s\n", err)
					os.Exit(1)
				}
			}

			options := &bare.Options{
				LogErrors:    errors,
				LocalAddress: localAddress,
				Family:       family,
				Maintainer:   maintainerData,
			}

			bareServer := bare.CreateBareServer(directory, options)

			fmt.Printf("Error Logging: %t\n", errors)
			fmt.Printf("URL:           http://%s:%d%s\n", host, port, directory)
			if maintainerData != nil {
				fmt.Printf("Maintainer:    %s\n", maintainerData)
			}

			if err := bareServer.Start(fmt.Sprintf("%s:%d", host, port)); err != nil {
				fmt.Fprintf(os.Stderr, "Error starting server: %s\n", err)
				os.Exit(1)
			}
		},
	}

	rootCmd.Flags().StringVarP(&directory, "directory", "d", "/", "Bare directory")
	rootCmd.Flags().StringVarP(&host, "host", "o", "0.0.0.0", "Listening host")
	rootCmd.Flags().IntVarP(&port, "port", "p", 80, "Listening port")
	rootCmd.Flags().BoolVarP(&errors, "errors", "e", false, "Error logging")
	rootCmd.Flags().StringVarP(&localAddress, "local-address", "a", "", "Address/network interface")
	rootCmd.Flags().IntVarP(&family, "family", "f", 0, "IP address family used when looking up host/hostnames. Default is 0 (both IPv4 and IPv6)")
	rootCmd.Flags().StringVarP(&maintainer, "maintainer", "m", "", "Inline maintainer data (JSON)")
	rootCmd.Flags().StringVarP(&maintainerFile, "maintainer-file", "j", "", "Path to maintainer data (JSON)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
