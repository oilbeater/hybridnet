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

package strategy

import (
	"strings"

	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	statefulWorkloadKindVar  = "StatefulSet"
	statelessWorkloadKindVar = ""
	StatefulWorkloadKind     map[string]bool
	StatelessWorkloadKind    map[string]bool
	DefaultIPRetain          bool
)

func init() {
	logger := log.Log.WithName("strategy")

	pflag.BoolVar(&DefaultIPRetain, "default-ip-retain", true, "Whether pod IP of stateful workloads will be retained by default.")
	pflag.StringVar(&statefulWorkloadKindVar, "stateful-workload-kinds", statefulWorkloadKindVar, `stateful workload kinds to use strategic IP allocation,`+
		`eg: "StatefulSet,AdvancedStatefulSet", default: "StatefulSet"`)
	pflag.StringVar(&statelessWorkloadKindVar, "stateless-workload-kinds", statelessWorkloadKindVar, "stateless workload kinds to use strategic IP allocation,"+
		`eg: "ReplicaSet", default: ""`)

	StatefulWorkloadKind = make(map[string]bool)
	StatelessWorkloadKind = make(map[string]bool)

	for _, kind := range strings.Split(statefulWorkloadKindVar, ",") {
		if len(kind) > 0 {
			StatefulWorkloadKind[kind] = true
			logger.Info("Adding kind to known stateful workloads", "kind", kind)
		}
	}

	for _, kind := range strings.Split(statelessWorkloadKindVar, ",") {
		if len(kind) > 0 {
			StatelessWorkloadKind[kind] = true
			logger.Info("Adding kind to known stateless workloads", "kind", kind)
		}
	}
}

func OwnByStatefulWorkload(pod *v1.Pod) bool {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false
	}

	return StatefulWorkloadKind[ref.Kind]
}

func OwnByStatelessWorkload(pod *v1.Pod) bool {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false
	}

	return StatelessWorkloadKind[ref.Kind]
}

func GetKnownOwnReference(pod *v1.Pod) *metav1.OwnerReference {
	// only support stateful workloads
	if OwnByStatefulWorkload(pod) {
		return metav1.GetControllerOf(pod)
	}
	return nil
}
