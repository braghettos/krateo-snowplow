package api

import (
	"fmt"

	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
)

func topologicalSort(items []*templates.API) ([]string, error) {
	graph := make(map[string][]string)
	inDegree := make(map[string]int)
	itemSet := make(map[string]bool)

	for _, item := range items {
		itemSet[item.Name] = true

		if item.DependsOn == nil {
			continue
		}

		if dep := item.DependsOn.Name; len(dep) > 0 {
			graph[dep] = append(graph[dep], item.Name)
			inDegree[item.Name]++
		}
	}

	var queue []string
	for item := range itemSet {
		if inDegree[item] == 0 {
			queue = append(queue, item)
		}
	}

	var sortedItems []string
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		sortedItems = append(sortedItems, item)

		for _, dependent := range graph[item] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sortedItems) != len(itemSet) {
		return nil, fmt.Errorf("cyclic dependency detected")
	}

	return sortedItems, nil
}
