package main

import (
	"fmt"
	"os"
	"path/filepath"

	bcli "github.com/filecoin-project/boost/cli"
	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
)

var importDirectDataCmd = &cli.Command{
	Name:      "import-direct",
	Usage:     "Import data for direct onboarding flow with Boost",
	ArgsUsage: "<piececid> <file>",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "delete-after-import",
			Usage: "whether to delete the data for the import after the data has been added to a sector",
			Value: false,
		},
		&cli.StringFlag{
			Name:     "client-addr",
			Usage:    "",
			Required: true,
		},
		&cli.Uint64Flag{
			Name:     "allocation-id",
			Usage:    "",
			Required: true,
		},
		&cli.BoolFlag{
			Name:  "fast-retrieval",
			Usage: "indicates that data should be available for fast retrieval",
			Value: true,
		},
		&cli.BoolFlag{
			Name:  "skip-ipni-announce",
			Usage: "indicates that deal index should not be announced to the IPNI(Network Indexer)",
			Value: false,
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.Args().Len() < 2 {
			return fmt.Errorf("must specify piececid and file path")
		}

		piececidStr := cctx.Args().Get(0)
		path := cctx.Args().Get(1)

		fullpath, err := homedir.Expand(path)
		if err != nil {
			return fmt.Errorf("expanding file path: %w", err)
		}

		filepath, err := filepath.Abs(fullpath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path for file: %w", err)
		}

		_, err = os.Stat(filepath)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", filepath, err)
		}

		piececid, err := cid.Decode(piececidStr)
		if err != nil {
			return fmt.Errorf("could not parse piececid: %w", err)
		}

		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		clientAddr, err := address.NewFromString(cctx.String("client-addr"))
		if err != nil {
			return fmt.Errorf("failed to parse clientaddr param: %w", err)
		}

		allocationId := cctx.Uint64("allocation-id")

		rej, err := napi.BoostDirectDeal(cctx.Context, piececid, filepath, cctx.Bool("delete-after-import"), allocationId, clientAddr, cctx.Bool("fast-retrieval"), cctx.Bool("skip-ipni-announce"))
		if err != nil {
			return fmt.Errorf("failed to execute direct data import: %w", err)
		}
		if rej != nil && rej.Reason != "" {
			return fmt.Errorf("direct data import rejected: %s", rej.Reason)
		}
		fmt.Println("Direct data import scheduled for execution")
		return nil
	},
}
