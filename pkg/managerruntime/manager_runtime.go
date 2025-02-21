/*
 Copyright 2021 The Hybridnet Authors.

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package managerruntime

import (
	"sync"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type managerRuntime struct {
	manager.Manager
	name string
	daemon
}

func NewManagerRuntime(name string, config *rest.Config, options *manager.Options) (ManagerRuntime, error) {
	manager, err := manager.New(config, *options)
	if err != nil {
		return nil, err
	}

	return &managerRuntime{
		Manager: manager,
		name:    name,
		daemon: daemon{
			mutex:        sync.Mutex{},
			logger:       manager.GetLogger().WithName(name),
			startFunc:    manager.Start,
			daemonStatus: daemonStatus{},
		},
	}, nil
}
