/*
Copyright 2019 The Kubernetes Authors.

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

package hetzner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/hetzner/hcloud-go/hcloud"
	"os"
	"strings"
	"text/template"
)

var (
	version = "dev"
)

var nameTemplateFuncMap = template.FuncMap{
	"compare": strings.Compare,
	"contains": strings.Contains,
	"containsAny": strings.ContainsAny,
	"containsRune": strings.ContainsRune,
	"count": strings.Count,
	"equalFold": strings.EqualFold,
	"fields": strings.Fields,
	//"fieldsFunc": strings.FieldsFunc,
	"hasPrefix": strings.HasPrefix,
	"hasSuffix": strings.HasSuffix,
	//"index": strings.Index,
	"indexAny": strings.IndexAny,
	"indexByte": strings.IndexByte,
	//"indexFunc": strings.IndexFunc,
	"indexRune": strings.IndexRune,
	"join": strings.Join,
	"lastIndex": strings.LastIndex,
	"lastIndexAny": strings.LastIndexAny,
	"lastIndexByte": strings.LastIndexByte,
	//"lastIndexFunc": strings.LastIndexFunc,
	//"map": strings.Map,
	"repeat": strings.Repeat,
	"replace": strings.Replace,
	"replaceAll": strings.ReplaceAll,
	"split": strings.Split,
	"splitAfter": strings.SplitAfter,
	"splitAfterN": strings.SplitAfterN,
	"splitN": strings.SplitN,
	//"title": strings.Title,
	"toLower": strings.ToLower,
	//"toLowerSpecial": strings.ToLowerSpecial,
	"toTitle": strings.ToTitle,
	//"toTitleSpecial": strings.ToTitleSpecial,
	"toUpper": strings.ToUpper,
	//"toUpperSpecial": strings.ToUpperSpecial,
	"toValidUTF8": strings.ToValidUTF8,
	"trim": strings.Trim,
	//"trimFunc": strings.TrimFunc,
	"trimLeft": strings.TrimLeft,
	//"trimLeftFunc": strings.TrimLeftFunc,
	"trimPrefix": strings.TrimPrefix,
	"trimRight": strings.TrimRight,
	//"trimRightFunc": strings.TrimRightFunc,
	"trimSpace": strings.TrimSpace,
	"trimSuffix": strings.TrimSuffix,
}

// hetznerManager handles Hetzner communication and data caching of
// node groups
type hetznerManager struct {
	client         *hcloud.Client
	nodeGroups     map[string]*hetznerNodeGroup
	apiCallContext context.Context
	cloudInit      string
	image          string
	nameTemplate   *template.Template
	sshKeys        []string
}

func newManager() (*hetznerManager, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, errors.New("`HCLOUD_TOKEN` is not specified")
	}

	cloudInitBase64 := os.Getenv("HCLOUD_CLOUD_INIT")
	if cloudInitBase64 == "" {
		return nil, errors.New("`HCLOUD_CLOUD_INIT` is not specified")
	}

	image := os.Getenv("HCLOUD_IMAGE")
	if image == "" {
		image = "ubuntu-20.04"
	}

	var nameTemplate *template.Template

	if nameTemplateStr := os.Getenv("HCLOUD_NAME_TEMPLATE"); nameTemplateStr != "" {
		tpl, err := template.New("nameTemplate").Funcs(nameTemplateFuncMap).Parse(nameTemplateStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse name template: %s", err)
		}

		nameTemplate = tpl
	}

	sshKeys := strings.Split(os.Getenv("HCLOUD_SSH_KEY"), ",")

	client := hcloud.NewClient(hcloud.WithToken(token))
	ctx := context.Background()
	cloudInit, err := base64.StdEncoding.DecodeString(cloudInitBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cloud init error: %s", err)
	}

	m := &hetznerManager{
		client:         client,
		nodeGroups:     make(map[string]*hetznerNodeGroup),
		cloudInit:      string(cloudInit),
		nameTemplate:   nameTemplate,
		image:          image,
		sshKeys:        sshKeys,
		apiCallContext: ctx,
	}

	m.nodeGroups[drainingNodePoolId] = &hetznerNodeGroup{
		manager:      m,
		instanceType: "cx11",
		region:       "fsn1",
		targetSize:   0,
		maxSize:      0,
		minSize:      0,
		id:           drainingNodePoolId,
	}

	return m, nil
}

// Refresh refreshes the cache holding the nodegroups. This is called by the CA
// based on the `--scan-interval`. By default it's 10 seconds.
func (m *hetznerManager) Refresh() error {
	return nil
}

func (m *hetznerManager) allServers(nodeGroup string) ([]*hcloud.Server, error) {
	listOptions := hcloud.ListOpts{
		PerPage:       50,
		LabelSelector: nodeGroupLabel + "=" + nodeGroup,
	}

	requestOptions := hcloud.ServerListOpts{ListOpts: listOptions}
	servers, err := m.client.Server.AllWithOpts(m.apiCallContext, requestOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get servers for hcloud: %v", err)
	}

	return servers, nil
}

func (m *hetznerManager) deleteByNode(node *apiv1.Node) error {
	server, err := m.serverForNode(node)
	if err != nil {
		return fmt.Errorf("failed to delete node %s error: %v", node.Name, err)
	}

	if server == nil {
		return fmt.Errorf("failed to delete node %s server not found", node.Name)
	}

	return m.deleteServer(server)
}

func (m *hetznerManager) deleteServer(server *hcloud.Server) error {
	_, err := m.client.Server.Delete(m.apiCallContext, server)
	return err
}

func (m *hetznerManager) addNodeToDrainingPool(node *apiv1.Node) (*hetznerNodeGroup, error) {
	m.nodeGroups[drainingNodePoolId].targetSize += 1
	return m.nodeGroups[drainingNodePoolId], nil
}

func (m *hetznerManager) serverForNode(node *apiv1.Node) (*hcloud.Server, error) {
	var nodeIdOrName string
	if node.Spec.ProviderID != "" {
		nodeIdOrName = strings.TrimPrefix(node.Spec.ProviderID, providerIDPrefix)
	} else {
		nodeIdOrName = node.Name
	}

	server, _, err := m.client.Server.Get(m.apiCallContext, nodeIdOrName)
	if err != nil {
		return nil, fmt.Errorf("failed to get servers for node %s error: %v", node.Name, err)
	}
	return server, nil
}
