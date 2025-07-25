// Copyright © 2020 The Knative Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"errors"
	"fmt"

	clientserving "knative.dev/client/pkg/serving"

	"sort"
	"strconv"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"

	clientv1alpha1 "knative.dev/client/pkg/apis/client/v1alpha1"
	"knative.dev/client/pkg/commands"
	clientservingv1 "knative.dev/client/pkg/serving/v1"
	"knative.dev/serving/pkg/apis/serving"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
)

// IgnoredServiceAnnotations defines the annotation keys which should be
// removed from service annotations before export
var IgnoredServiceAnnotations = []string{
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
	"kubectl.kubernetes.io/last-applied-configuration",
}

// IgnoredRevisionAnnotations defines the annotation keys which should be
// removed from revision annotations before export
var IgnoredRevisionAnnotations = []string{
	"serving.knative.dev/lastPinned",
	"serving.knative.dev/creator",
	"serving.knative.dev/routingStateModified",
	clientserving.UpdateTimestampAnnotationKey,
}

// IgnoredServiceLabels defines the label keys which should be removed
// from service labels before export
var IgnoredServiceLabels = []string{
	"serving.knative.dev/configurationUID",
	"serving.knative.dev/serviceUID",
}

// IgnoredRevisionLabels defines the label keys which should be removed
// from revision labels before export
var IgnoredRevisionLabels = []string{
	"serving.knative.dev/configurationUID",
	"serving.knative.dev/serviceUID",
}

const (
	ModeReplay = "replay"
	ModeExport = "export"
)

// NewServiceExportCommand returns a new command for exporting a service.
func NewServiceExportCommand(p *commands.KnParams) *cobra.Command {

	// For machine readable output
	machineReadablePrintFlags := genericclioptions.NewPrintFlags("")

	command := &cobra.Command{
		Use:   "export NAME",
		Short: "Export a service and its revisions",
		Example: `
  # Export a service in YAML format (Beta)
  kn service export foo -n bar -o yaml

  # Export a service in JSON format (Beta)
  kn service export foo -n bar -o json

  # Export a service with revisions (Beta)
  kn service export foo --with-revisions --mode=export -n bar -o json

  # Export services in kubectl friendly format, as a list kind, one service item for each revision (Beta)
  kn service export foo --with-revisions --mode=replay -n bar -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("'kn service export' requires name of the service as single argument")
			}
			if !machineReadablePrintFlags.OutputFlagSpecified() {
				return errors.New("'kn service export' requires output format")
			}
			serviceName := args[0]

			namespace, err := p.GetNamespace(cmd)
			if err != nil {
				return err
			}

			client, err := p.NewServingClient(namespace)
			if err != nil {
				return err
			}

			service, err := client.GetService(cmd.Context(), serviceName)
			if err != nil {
				return err
			}
			printer, err := machineReadablePrintFlags.ToPrinter()
			if err != nil {
				return err
			}
			return exportService(cmd, service, client, printer)
		},
	}
	flags := command.Flags()
	commands.AddNamespaceFlags(flags, false)
	flags.Bool("with-revisions", false, "Export all routed revisions (Beta)")
	flags.String("mode", "", "Format for exporting all routed revisions. One of replay|export (Beta)")
	machineReadablePrintFlags.AddFlags(command)
	return command
}

func exportService(cmd *cobra.Command, service *servingv1.Service, client clientservingv1.KnServingClient, printer printers.ResourcePrinter) error {
	withRevisions, err := cmd.Flags().GetBool("with-revisions")
	if err != nil {
		return err
	}

	mode, err := cmd.Flags().GetString("mode")
	if err != nil {
		return err
	}

	if mode == ModeReplay {
		svcList, err := exportServiceListForReplay(cmd.Context(), service.DeepCopy(), client, withRevisions)
		if err != nil {
			return err
		}
		return printer.PrintObj(svcList, cmd.OutOrStdout())
	}
	// default is export mode
	knExport, err := exportForKNImport(cmd.Context(), service.DeepCopy(), client, withRevisions)
	if err != nil {
		return err
	}
	//print kn export
	return printer.PrintObj(knExport, cmd.OutOrStdout())
}

func exportLatestService(latestSvc *servingv1.Service, withRoutes bool) *servingv1.Service {
	exportedSvc := servingv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        latestSvc.ObjectMeta.Name,
			Labels:      latestSvc.ObjectMeta.Labels,
			Annotations: latestSvc.ObjectMeta.Annotations,
		},
		TypeMeta: latestSvc.TypeMeta,
	}

	exportedSvc.Spec.Template = servingv1.RevisionTemplateSpec{
		Spec:       latestSvc.Spec.Template.Spec,
		ObjectMeta: latestSvc.Spec.Template.ObjectMeta,
	}

	if withRoutes {
		exportedSvc.Spec.RouteSpec = latestSvc.Spec.RouteSpec
		for i := range exportedSvc.Spec.RouteSpec.Traffic {
			if exportedSvc.Spec.RouteSpec.Traffic[i].LatestRevision != nil && *exportedSvc.Spec.RouteSpec.Traffic[i].LatestRevision {
				exportedSvc.Spec.RouteSpec.Traffic[i].RevisionName = latestSvc.Status.LatestReadyRevisionName
			}
		}
	}

	stripIgnoredAnnotationsFromService(&exportedSvc)
	stripIgnoredLabelsFromService(&exportedSvc)
	stripIgnoredAnnotationsFromRevisionTemplate(&exportedSvc.Spec.Template)
	stripIgnoredLabelsFromRevisionTemplate(&exportedSvc.Spec.Template)
	return &exportedSvc
}

func exportRevision(revision *servingv1.Revision) servingv1.Revision {
	exportedRevision := servingv1.Revision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        revision.ObjectMeta.Name,
			Labels:      revision.ObjectMeta.Labels,
			Annotations: revision.ObjectMeta.Annotations,
		},
		TypeMeta: revision.TypeMeta,
	}

	exportedRevision.Spec = revision.Spec
	stripIgnoredAnnotationsFromRevision(&exportedRevision)
	stripIgnoredLabelsFromRevision(&exportedRevision)
	return exportedRevision
}

func constructServiceFromRevision(latestSvc *servingv1.Service, revision *servingv1.Revision) servingv1.Service {
	exportedSvc := servingv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        latestSvc.ObjectMeta.Name,
			Labels:      latestSvc.ObjectMeta.Labels,
			Annotations: latestSvc.ObjectMeta.Annotations,
		},
		TypeMeta: latestSvc.TypeMeta,
	}
	exportedSvc.Spec.Template = servingv1.RevisionTemplateSpec{
		Spec:       revision.Spec,
		ObjectMeta: latestSvc.Spec.Template.ObjectMeta,
	}

	//overriding revision template annotations with revision annotations
	stripIgnoredAnnotationsFromRevision(revision)
	exportedSvc.Spec.Template.ObjectMeta.Annotations = revision.ObjectMeta.Annotations

	exportedSvc.Spec.Template.ObjectMeta.Name = revision.ObjectMeta.Name
	stripIgnoredAnnotationsFromService(&exportedSvc)
	return exportedSvc
}

func exportServiceListForReplay(ctx context.Context, latestSvc *servingv1.Service, client clientservingv1.KnServingClient, withRevisions bool) (runtime.Object, error) {
	if !withRevisions {
		return exportLatestService(latestSvc, false), nil
	}
	var exportedSvcItems []servingv1.Service

	revisionList, revsMap, err := getRevisionsToExport(ctx, latestSvc, client)
	if err != nil {
		return nil, err
	}

	for _, revision := range revisionList.Items {
		//construct service only for active revisions
		if revsMap[revision.ObjectMeta.Name] && revision.ObjectMeta.Name != latestSvc.Spec.Template.ObjectMeta.Name {
			exportedSvcItems = append(exportedSvcItems, constructServiceFromRevision(latestSvc, revision.DeepCopy()))
		}
	}

	//add latest service, add traffic if more than one revision exist
	exportedSvcItems = append(exportedSvcItems, *(exportLatestService(latestSvc, len(revisionList.Items) > 1)))

	typeMeta := metav1.TypeMeta{
		APIVersion: "v1",
		Kind:       "List",
	}
	exportedSvcList := &servingv1.ServiceList{
		TypeMeta: typeMeta,
		Items:    exportedSvcItems,
	}

	return exportedSvcList, nil
}

func exportForKNImport(ctx context.Context, latestSvc *servingv1.Service, client clientservingv1.KnServingClient, withRevisions bool) (*clientv1alpha1.Export, error) {
	var exportedRevItems []servingv1.Revision
	revisionHistoryCount := 0
	if withRevisions {
		revisionList, revsMap, err := getRevisionsToExport(ctx, latestSvc, client)
		if err != nil {
			return nil, err
		}

		for _, revision := range revisionList.Items {
			//append only active revisions, no latest revision
			if revsMap[revision.ObjectMeta.Name] && revision.ObjectMeta.Name != latestSvc.Spec.Template.ObjectMeta.Name {
				exportedRevItems = append(exportedRevItems, exportRevision(revision.DeepCopy()))
			}
		}
		revisionHistoryCount = len(revisionList.Items)
	}

	typeMeta := metav1.TypeMeta{
		APIVersion: "client.knative.dev/v1alpha1",
		Kind:       "Export",
	}
	knExport := &clientv1alpha1.Export{
		TypeMeta: typeMeta,
		Spec: clientv1alpha1.ExportSpec{
			Service:   *(exportLatestService(latestSvc, revisionHistoryCount > 1)),
			Revisions: exportedRevItems,
		},
	}

	return knExport, nil
}

func getRevisionsToExport(ctx context.Context, latestSvc *servingv1.Service, client clientservingv1.KnServingClient) (*servingv1.RevisionList, map[string]bool, error) {
	//get revisions to export from traffic
	revsMap := getRoutedRevisions(latestSvc)

	// Query for list with filters
	revisionList, err := client.ListRevisions(ctx, clientservingv1.WithService(latestSvc.ObjectMeta.Name))
	if err != nil {
		return nil, nil, err
	}
	if len(revisionList.Items) == 0 {
		return nil, nil, fmt.Errorf("no revisions found for the service %s", latestSvc.ObjectMeta.Name)
	}
	// sort revisions to maintain the order of generations
	sortRevisions(revisionList)
	return revisionList, revsMap, nil
}

func getRoutedRevisions(latestSvc *servingv1.Service) map[string]bool {
	trafficList := latestSvc.Spec.RouteSpec.Traffic
	revsMap := make(map[string]bool)

	for _, traffic := range trafficList {
		if traffic.RevisionName != "" {
			revsMap[traffic.RevisionName] = true
		}
	}
	return revsMap
}

// sortRevisions sorts revisions by generation and name (in this order)
func sortRevisions(revisionList *servingv1.RevisionList) {
	// sort revisionList by configuration generation key
	sort.SliceStable(revisionList.Items, revisionListSortFunc(revisionList))
}

// revisionListSortFunc sorts by generation and name
func revisionListSortFunc(revisionList *servingv1.RevisionList) func(i int, j int) bool {
	return func(i, j int) bool {
		a := revisionList.Items[i]
		b := revisionList.Items[j]

		// By Generation
		// Convert configuration generation key from string to int for avoiding string comparison.
		agen, err := strconv.Atoi(a.Labels[serving.ConfigurationGenerationLabelKey])
		if err != nil {
			return a.Name > b.Name
		}
		bgen, err := strconv.Atoi(b.Labels[serving.ConfigurationGenerationLabelKey])
		if err != nil {
			return a.Name > b.Name
		}

		if agen != bgen {
			return agen < bgen
		}
		return a.Name > b.Name
	}
}

func stripIgnoredAnnotationsFromService(svc *servingv1.Service) {
	for _, annotation := range IgnoredServiceAnnotations {
		delete(svc.ObjectMeta.Annotations, annotation)
	}
}

func stripIgnoredAnnotationsFromRevision(revision *servingv1.Revision) {
	for _, annotation := range IgnoredRevisionAnnotations {
		delete(revision.ObjectMeta.Annotations, annotation)
	}
}

func stripIgnoredAnnotationsFromRevisionTemplate(template *servingv1.RevisionTemplateSpec) {
	for _, annotation := range IgnoredRevisionAnnotations {
		delete(template.ObjectMeta.Annotations, annotation)
	}
}

func stripIgnoredLabelsFromService(svc *servingv1.Service) {
	for _, label := range IgnoredServiceLabels {
		delete(svc.ObjectMeta.Labels, label)
	}
}

func stripIgnoredLabelsFromRevision(rev *servingv1.Revision) {
	for _, label := range IgnoredRevisionLabels {
		delete(rev.ObjectMeta.Labels, label)
	}
}

func stripIgnoredLabelsFromRevisionTemplate(template *servingv1.RevisionTemplateSpec) {
	for _, label := range IgnoredRevisionLabels {
		delete(template.ObjectMeta.Labels, label)
	}
}
