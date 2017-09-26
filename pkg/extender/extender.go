package extender

import (
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/pkg/api/v1"
)

const (
	TotalVFsResource v1.ResourceName = "totalvfs"
)

var (
	singleItem = resource.NewQuantity(1, resource.DecimalSI)
	zero       = resource.NewQuantity(0, resource.DecimalSI)
)

func NewExtender() *Extender {
	return &Extender{
		allocatedVFs: make(map[string]*resource.Quantity),
		promises:     []time.Time{},
		promisedVFs:  resource.NewQuantity(0, resource.DecimalSI),
		selector:     NetworkSelector,
	}
}

type Extender struct {
	sync.Mutex
	allocatedVFs map[string]*resource.Quantity

	// number of promises must be always equal to number of promised VFs
	// in separate loop we will go over promises and clear them as needed
	// promisedVFs are global because we cant guarantee that original scheduler
	// will choose first node from our order.
	promises    []time.Time
	promisedVFs *resource.Quantity

	selector Selector
}

func (ext *Extender) FilterArgs(args *ExtenderArgs) (*ExtenderFilterResult, error) {
	if !ext.selector(&args.Pod) {
		return nil, nil
	}
	ext.Lock()
	defer ext.Unlock()
	result := &ExtenderFilterResult{
		Nodes:       &v1.NodeList{Items: make([]v1.Node, 0, 1)},
		FailedNodes: make(map[string]string),
	}
	for _, node := range args.Nodes.Items {
		if _, exists := ext.allocatedVFs[node.Name]; !exists {
			ext.allocatedVFs[node.Name] = resource.NewQuantity(0, resource.DecimalExponent)
		}
		allocated := ext.allocatedVFs[node.Name]
		if res, exists := node.Status.Allocatable[TotalVFsResource]; !exists {
			log.Printf("No allocatable vfs on a node %s \n", node.Name)
			continue
		} else {
			log.Printf("Node %s has a total of %v allocatable vfs.", node.Name, res)
			res.Sub(*allocated)
			res.Sub(*ext.promisedVFs)
			if res.Cmp(*zero) != 1 {
				log.Printf(
					"Node %s has available VF and it will be promised to a pod %s/%s.",
					node.Name, args.Pod.Namespace, args.Pod.Name)
				result.Nodes.Items = append(result.Nodes.Items, node)
				ext.promisedVFs.Add(*singleItem)
				ext.promises = append(ext.promises, time.Now())
			} else {
				log.Printf("Node %s doesnt have sufficient number of VFs", node.Name)
				result.FailedNodes[node.Name] = fmt.Sprintf(
					"Not sufficient number of VFs. Allocated: %v. Promised: %v. Total: %v",
					allocated, ext.promisedVFs, res,
				)
			}
		}
	}
	if len(result.Nodes.Items) == 0 {
		result.Error = "No nodes have available VFs."
	}
	return result, nil
}
