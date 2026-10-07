package envcapture

// The minimal Kubernetes API JSON shapes this package reads. Only the fields the capture actually
// uses are declared — everything else in a real API response is ignored by json.Unmarshal, exactly
// like every other JSON reader in this codebase (e.g. internal/runner/autoscale.go's Deployment
// read).

type k8sOwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type k8sObjectMeta struct {
	Name            string        `json:"name"`
	OwnerReferences []k8sOwnerRef `json:"ownerReferences"`
}

type k8sResourceList struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type k8sContainer struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Resources struct {
		Requests k8sResourceList `json:"requests"`
		Limits   k8sResourceList `json:"limits"`
	} `json:"resources"`
}

type k8sContainerStatus struct {
	Name         string `json:"name"`
	ImageID      string `json:"imageID"`
	RestartCount int    `json:"restartCount"`
}

type k8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Spec     struct {
		NodeName   string         `json:"nodeName"`
		Containers []k8sContainer `json:"containers"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses []k8sContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type k8sPodList struct {
	Items []k8sPod `json:"items"`
}

type k8sReplicaSetList struct {
	Items []struct {
		Metadata k8sObjectMeta `json:"metadata"`
	} `json:"items"`
}

type k8sWorkload struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

type k8sWorkloadList struct {
	Items []k8sWorkload `json:"items"`
}

type k8sNode struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Status   struct {
		Allocatable k8sResourceList `json:"allocatable"`
	} `json:"status"`
}

type k8sNodeList struct {
	Items []k8sNode `json:"items"`
}
