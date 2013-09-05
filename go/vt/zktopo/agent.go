// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zktopo

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/zk"
	"launchpad.net/gozk/zookeeper"
)

/*
This file contains the code to support the local agent process for zktopo.Server
*/

func (zkts *Server) ValidateTabletActions(tabletAlias topo.TabletAlias) error {
	actionPath := TabletActionPathForAlias(tabletAlias)

	// Ensure that the action node is there. There is no conflict creating
	// this node.
	_, err := zkts.zconn.Create(actionPath, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil && !zookeeper.IsError(err, zookeeper.ZNODEEXISTS) {
		return err
	}
	return nil
}

func (zkts *Server) CreateTabletPidNode(tabletAlias topo.TabletAlias, contents string, done chan struct{}) error {
	zkTabletPath := TabletPathForAlias(tabletAlias)
	path := path.Join(zkTabletPath, "pid")
	return zk.CreatePidNode(zkts.zconn, path, contents, done)
}

func (zkts *Server) ValidateTabletPidNode(tabletAlias topo.TabletAlias) error {
	zkTabletPath := TabletPathForAlias(tabletAlias)
	path := path.Join(zkTabletPath, "pid")
	_, _, err := zkts.zconn.Get(path)
	return err
}

func (zkts *Server) GetSubprocessFlags() []string {
	return zk.GetZkSubprocessFlags()
}

func (zkts *Server) handleActionQueue(tabletAlias topo.TabletAlias, dispatchAction func(actionPath, data string) error) (<-chan zookeeper.Event, error) {
	zkActionPath := TabletActionPathForAlias(tabletAlias)

	// This read may seem a bit pedantic, but it makes it easier
	// for the system to trend towards consistency if an action
	// fails or somehow the action queue gets mangled by an errant
	// process.
	children, _, watch, err := zkts.zconn.ChildrenW(zkActionPath)
	if err != nil {
		return watch, err
	}
	if len(children) > 0 {
		sort.Strings(children)
		for _, child := range children {
			actionPath := zkActionPath + "/" + child
			if _, err := strconv.ParseUint(child, 10, 64); err != nil {
				// This is handy if you want to restart a stuck queue.
				// FIXME(msolomon) could listen on the queue node for a change
				// generated by a "touch", but listening on two things is a bit
				// more complex.
				log.Warningf("remove invalid event from action queue: %v", child)
				zkts.zconn.Delete(actionPath, -1)
			}

			data, _, err := zkts.zconn.Get(actionPath)
			if err != nil {
				log.Errorf("cannot read action %v from zk: %v", actionPath, err)
				break
			}

			if err := dispatchAction(actionPath, data); err != nil {
				break
			}
		}
	}
	return watch, nil
}

func (zkts *Server) ActionEventLoop(tabletAlias topo.TabletAlias, dispatchAction func(actionPath, data string) error, done chan struct{}) {
	for {
		// Process any pending actions when we startup, before we start listening
		// for events.
		watch, err := zkts.handleActionQueue(tabletAlias, dispatchAction)
		if err != nil {
			log.Warningf("action queue failed: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// FIXME(msolomon) Add a skewing timer here to guarantee we wakeup
		// periodically even if events are missed?
		select {
		case event := <-watch:
			if !event.Ok() {
				// NOTE(msolomon) The zk meta conn will reconnect automatically, or
				// error out. At this point, there isn't much to do.
				log.Warningf("zookeeper not OK: %v", event)
				time.Sleep(5 * time.Second)
			}
			// Otherwise, just handle the queue above.
		case <-done:
			return
		}
	}
}

// actionPathToTabletAlias parses an actionPath back
// zkActionPath is /zk/<cell>/vt/tablets/<uid>/action/<number>
func actionPathToTabletAlias(actionPath string) (topo.TabletAlias, error) {
	pathParts := strings.Split(actionPath, "/")
	if len(pathParts) != 8 || pathParts[0] != "" || pathParts[1] != "zk" || pathParts[3] != "vt" || pathParts[4] != "tablets" || pathParts[6] != "action" {
		return topo.TabletAlias{}, fmt.Errorf("invalid action path: %v", actionPath)
	}
	return topo.ParseTabletAliasString(pathParts[2] + "-" + pathParts[5])
}

func (zkts *Server) ReadTabletActionPath(actionPath string) (topo.TabletAlias, string, int64, error) {
	tabletAlias, err := actionPathToTabletAlias(actionPath)
	if err != nil {
		return topo.TabletAlias{}, "", 0, err
	}

	data, stat, err := zkts.zconn.Get(actionPath)
	if err != nil {
		return topo.TabletAlias{}, "", 0, err
	}

	return tabletAlias, data, int64(stat.Version()), nil
}

func (zkts *Server) UpdateTabletAction(actionPath, data string, version int64) error {
	_, err := zkts.zconn.Set(actionPath, data, int(version))
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZBADVERSION) {
			err = topo.ErrBadVersion
		}
		return err
	}
	return nil
}

// StoreTabletActionResponse stores the data both in action and actionlog
func (zkts *Server) StoreTabletActionResponse(actionPath, data string) error {
	_, err := zkts.zconn.Set(actionPath, data, -1)
	if err != nil {
		return err
	}

	actionLogPath := strings.Replace(actionPath, "/action/", "/actionlog/", 1)
	_, err = zk.CreateRecursive(zkts.zconn, actionLogPath, data, 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	return err
}

func (zkts *Server) UnblockTabletAction(actionPath string) error {
	return zkts.zconn.Delete(actionPath, -1)
}
