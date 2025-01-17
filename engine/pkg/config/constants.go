// Copyright 2022 PingCAP, Inc.
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

package config

import (
	"time"
)

// Define some constants
const (
	ServerMasterEtcdDialTimeout = 5 * time.Second
	// Disable endpoints auto sync in etcd client. Make sure to pass a load
	// balancer address(such as service endpoint in K8s), or all advertise-addrs
	// of the etcd cluster.
	ServerMasterEtcdSyncInterval = time.Duration(0)
)
