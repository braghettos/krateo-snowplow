package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RESTActionSpec defines the api handler specifications.
type RESTActionSpec struct {
	//+listType=atomic
	API    []*API  `json:"api,omitempty"`
	Filter *string `json:"filter,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:scope=Namespaced,shortName=ra,categories={krateo,rest,actions}

// RESTAction allows users to declaratively define calls to APIs that may in turn depend on other calls.
type RESTAction struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec   RESTActionSpec        `json:"spec"`
	Status *runtime.RawExtension `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RESTActionList contains a list of RESTAction
type RESTActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []RESTAction `json:"items"`
}
