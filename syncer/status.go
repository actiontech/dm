// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"github.com/pingcap/tidb-enterprise-tools/dm/pb"
	"github.com/siddontang/go-mysql/mysql"
)

// Status implements SubTaskUnit.Status
// it returns status, but does not calc status
func (s *Syncer) Status() interface{} {
	var (
		masterPos     mysql.Position
		masterGTIDSet GTIDSet
	)
	total := s.count.Get()
	totalTps := s.totalTps.Get()
	tps := s.totalTps.Get()
	if !s.lackOfReplClientPrivilege {
		masterPos, masterGTIDSet, _ = s.getMasterStatus()
	}
	syncerPos := s.meta.Pos()
	syncerGTIDSet, _ := s.meta.GTID()
	st := &pb.SyncStatus{
		TotalEvents:      total,
		TotalTps:         totalTps,
		RecentTps:        tps,
		MasterBinlog:     masterPos.String(),
		MasterBinlogGtid: masterGTIDSet.String(),
		SyncerBinlog:     syncerPos.String(),
		SyncerBinlogGtid: syncerGTIDSet.String(),
	}
	return st
}
