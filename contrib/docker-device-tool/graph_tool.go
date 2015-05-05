// Copyright 2015 graph_tool authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Author <Ye Yin<hustcat@gmail.com>

package main

import (
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/devmapper"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/archive"
	"io"
	"os"
	"path"
)

var (
	root         string
	graphOptions []string = []string{"dm.basesize=20G", "dm.loopdatasize=200G"}
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s <flags>   [diff id parent]|[apply containerId]|[diffapply id parent containerId]\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func initGraph() (*graph.Graph, error) {
	graphdriver.Register("devicemapper", devmapper.Init)
	graphdriver.DefaultDriver = "devicemapper"

	// Load storage driver
	driver, err := graphdriver.New(root, graphOptions)
	if err != nil {
		log.Errorf("Load storage driver error: %v", err)
		return nil, err
	}
	log.Debugf("Using graph driver %s", driver)

	log.Debugf("Creating images graph")
	g, err := graph.NewGraph(path.Join(root, "graph"), driver)
	if err != nil {
		log.Errorf("Creating images graph error: %v", err)
		return nil, err
	}
	return g, nil
}

func checkIsParent(id, parent string, g *graph.Graph) (bool, error) {
	isParent := false
	img, err := g.Get(id)
	if err != nil {
		return false, err
	}

	for {
		if img.Parent == "" {
			break
		}

		if img.Parent == parent {
			isParent = true
			break
		}

		img, err = g.Get(img.Parent)
		if err != nil {
			break
		}
	}
	return isParent, err
}

func exportDiff(id, parent string) error {
	g, err := initGraph()
	if err != nil {
		return err
	}

	b, err := checkIsParent(id, parent, g)
	if err != nil {
		return err
	}
	if !b {
		return fmt.Errorf("%s is not parent of %s", parent, id)
	}

	driver := g.Driver()
	fs, err := driver.Diff(id, parent)
	if err != nil {
		return err
	}
	defer fs.Close()

	_, err = io.Copy(os.Stdout, fs)
	if err != nil {
		return err
	}
	return nil
}

func applyLayer(containerId string) error {
	fs := os.Stdin
	dest := path.Join(root, "devicemapper", "mnt", containerId, "rootfs")
	fi, err := os.Stat(dest)
	if err != nil && !os.IsExist(err) {
		return err
	}

	if !fi.IsDir() {
		return fmt.Errorf(" Dest %s is not dir", dest)
	}

	err = archive.ApplyLayer(dest, fs)
	return err
}

func diffAndApply(id, parent, container string) error {
	g, err := initGraph()
	if err != nil {
		return err
	}

	b, err := checkIsParent(id, parent, g)
	if err != nil {
		return err
	}
	if !b {
		return fmt.Errorf("%s is not parent of %s", parent, id)
	}

	dest := path.Join(root, "devicemapper", "mnt", container, "rootfs")
	fi, err := os.Stat(dest)
	if err != nil && !os.IsExist(err) {
		return err
	}

	if !fi.IsDir() {
		return fmt.Errorf(" Dest %s is not dir", dest)
	}

	driver := g.Driver()
	fs, err := driver.Diff(id, parent)
	if err != nil {
		return err
	}
	defer fs.Close()

	err = archive.ApplyLayer(dest, fs)
	if err != nil {
		return err
	}
	return nil
}

func main() {
	flag.StringVar(&root, "r", "/var/lib/docker", "Docker root dir")
	opts.ListVar(&graphOptions, []string{"-storage-opt"}, "Set storage driver options")
	flDebug := flag.Bool("D", false, "Debug mode")

	flag.Parse()

	if *flDebug {
		//os.Setenv("DEBUG", "1")
		log.SetLevel(log.DebugLevel)
	}

	if flag.NArg() < 1 {
		usage()
	}

	args := flag.Args()

	switch args[0] {
	case "diff":
		if flag.NArg() < 3 {
			usage()
		}
		err := exportDiff(args[1], args[2])
		if err != nil {
			log.Errorf("Export diff  error: %v", err)
			os.Exit(1)
		}
		break
	case "diffapply":
		if flag.NArg() < 4 {
			usage()
		}
		err := diffAndApply(args[1], args[2], args[3])
		if err != nil {
			log.Errorf("Apply diff  error: %v", err)
			os.Exit(1)
		} else {
			log.Infof("Apply diff success")
		}
		break
	case "apply":
		if flag.NArg() < 2 {
			usage()
		}
		err := applyLayer(args[1])
		if err != nil {
			log.Errorf("Apply diff  error: %v", err)
			os.Exit(1)
		} else {
			log.Infof("Apply diff success")
		}
		break
	default:
		fmt.Printf("Unknown command %s\n", args[0])
		usage()

		os.Exit(1)
	}
}
