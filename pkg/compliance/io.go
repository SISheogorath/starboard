package compliance

import (
	"context"
	"fmt"
	"strings"

	"github.com/aquasecurity/starboard/pkg/apis/aquasecurity/v1alpha1"
	"github.com/aquasecurity/starboard/pkg/ext"
	"github.com/emirpasic/gods/sets/hashset"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Mgr interface {
	GenerateComplianceReport(ctx context.Context, spec v1alpha1.ReportSpec) (*v1alpha1.ClusterComplianceReport, error)
}

func NewMgr(client client.Client, log logr.Logger) Mgr {
	return &cm{
		client: client,
		log:    log,
	}
}

type cm struct {
	client client.Client
	log    logr.Logger
}

type summaryTotal struct {
	pass int
	fail int
}

type specDataMapping struct {
	scannerResourceListNames map[string]*hashset.Set
	controlIDControlObject   map[string]v1alpha1.Control
	controlCheckIds          map[string][]string
}

func (w *cm) GenerateComplianceReport(ctx context.Context, spec v1alpha1.ReportSpec) (*v1alpha1.ClusterComplianceReport, error) {
	// map specs to key/value map for easy processing
	smd := w.populateSpecDataToMaps(spec)
	// map compliance scanner to resource data
	scannerResourceMap := mapComplianceScannerToResource(w.client, ctx, smd.scannerResourceListNames)
	// organized data by check id and it aggregated results
	checkIdsToResults, err := w.checkIdsToResults(scannerResourceMap)
	if err != nil {
		return nil, err
	}
	// map scanner checks results to control check results
	controlChecks := w.controlChecksByScannerChecks(smd, checkIdsToResults)
	// find summary totals
	st := w.getTotals(controlChecks)
	//publish compliance details report
	err = w.createComplianceDetailReport(ctx, spec, smd, checkIdsToResults, st)
	if err != nil {
		return nil, err
	}
	//generate compliance details report
	return w.createComplianceReport(ctx, spec, st, controlChecks)

}

//createComplianceReport create compliance report
func (w *cm) createComplianceReport(ctx context.Context, spec v1alpha1.ReportSpec, st summaryTotal, controlChecks []v1alpha1.ControlCheck) (*v1alpha1.ClusterComplianceReport, error) {
	statusControlChecks := make([]v1alpha1.ControlCheck, 0)
	//check if status data should be updated
	if st.fail > 0 || st.pass > 0 {
		statusControlChecks = append(statusControlChecks, controlChecks...)
	}
	summary := v1alpha1.ClusterComplianceSummary{PassCount: st.pass, FailCount: st.fail}
	report := v1alpha1.ClusterComplianceReport{
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.ToLower(spec.Name),
		},
		Status: v1alpha1.ReportStatus{UpdateTimestamp: metav1.NewTime(ext.NewSystemClock().Now()), Summary: summary, ControlChecks: statusControlChecks},
	}
	var existing v1alpha1.ClusterComplianceReport
	err := w.client.Get(ctx, types.NamespacedName{
		Name: strings.ToLower(spec.Name),
	}, &existing)
	if err != nil {
		return nil, fmt.Errorf("compliance crd with name %s is missing", spec.Name)
	}
	copied := existing.DeepCopy()
	copied.Labels = report.Labels
	copied.Status = report.Status
	copied.Spec = spec
	copied.Status.UpdateTimestamp = metav1.NewTime(ext.NewSystemClock().Now())
	return copied, nil
}

//createComplianceDetailReport create and publish compliance details report
func (w *cm) createComplianceDetailReport(ctx context.Context, spec v1alpha1.ReportSpec, smd *specDataMapping, checkIdsToResults map[string][]*ScannerCheckResult, st summaryTotal) error {
	controlChecksDetails := w.controlChecksDetailsByScannerChecks(smd, checkIdsToResults)
	name := strings.ToLower(fmt.Sprintf("%s-%s", spec.Name, "details"))
	// compliance details report
	summary := v1alpha1.ClusterComplianceDetailSummary{PassCount: st.pass, FailCount: st.fail}
	report := v1alpha1.ClusterComplianceDetailReport{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Report: v1alpha1.ClusterComplianceDetailReportData{UpdateTimestamp: metav1.NewTime(ext.NewSystemClock().Now()), Summary: summary, Type: v1alpha1.Compliance{Kind: strings.ToLower(spec.Kind), Name: name, Description: strings.ToLower(spec.Description), Version: spec.Version}, ControlChecks: controlChecksDetails},
	}

	var existing v1alpha1.ClusterComplianceDetailReport
	err := w.client.Get(ctx, types.NamespacedName{
		Name: name,
	}, &existing)

	if err == nil {
		copied := existing.DeepCopy()
		copied.Labels = report.Labels
		copied.Report = report.Report
		copied.Report.UpdateTimestamp = metav1.NewTime(ext.NewSystemClock().Now())
		return w.client.Update(ctx, copied)
	}

	if errors.IsNotFound(err) {
		return w.client.Create(ctx, &report)
	}
	return nil
}

// getTotals return control check totals
func (w *cm) getTotals(controlChecks []v1alpha1.ControlCheck) summaryTotal {
	var totalFail, totalPass int
	if len(controlChecks) > 0 {
		for _, controlCheck := range controlChecks {
			totalFail = totalFail + controlCheck.FailTotal
			totalPass = totalPass + controlCheck.PassTotal
		}
	}
	return summaryTotal{fail: totalFail, pass: totalPass}
}

// controlChecksByScannerChecks build control checks list by parsing test results and mapping it to relevant scanner
func (w *cm) controlChecksByScannerChecks(smd *specDataMapping, checkIdsToResults map[string][]*ScannerCheckResult) []v1alpha1.ControlCheck {
	controlChecks := make([]v1alpha1.ControlCheck, 0)
	for controlID, checkIds := range smd.controlCheckIds {
		var passTotal, failTotal, total int
		for _, checkId := range checkIds {
			results, ok := checkIdsToResults[checkId]
			if ok {
				for _, checkResult := range results {
					for _, crd := range checkResult.Details {
						switch crd.Status {
						case Pass, Warn:
							passTotal++
						case Fail:
							failTotal++
						}
						total++
					}
				}
			}
		}
		control, ok := smd.controlIDControlObject[controlID]
		if ok {
			controlChecks = append(controlChecks, v1alpha1.ControlCheck{ID: controlID,
				Name:        control.Name,
				Description: control.Description,
				Severity:    control.Severity,
				PassTotal:   passTotal,
				FailTotal:   failTotal})
		}
	}
	return controlChecks
}

// controlChecksDetailsByScannerChecks build control checks with details list by parsing test results and mapping it to relevant tool
func (w *cm) controlChecksDetailsByScannerChecks(smd *specDataMapping, checkIdsToResults map[string][]*ScannerCheckResult) []v1alpha1.ControlCheckDetails {
	controlChecks := make([]v1alpha1.ControlCheckDetails, 0)
	for controlID, checkIds := range smd.controlCheckIds {
		control, ok := smd.controlIDControlObject[controlID]
		if ok {
			for _, checkId := range checkIds {
				results, ok := checkIdsToResults[checkId]
				if ok {
					ctta := make([]v1alpha1.ScannerCheckResult, 0)
					for _, checkResult := range results {
						var ctt v1alpha1.ScannerCheckResult
						rds := make([]v1alpha1.ResultDetails, 0)
						for _, crd := range checkResult.Details {
							rds = append(rds, v1alpha1.ResultDetails{Name: crd.Name, Namespace: crd.Namespace, Msg: crd.Msg, Status: crd.Status})
						}
						ctt = v1alpha1.ScannerCheckResult{ID: checkResult.ID, ObjectType: checkResult.ObjectType, Remediation: checkResult.Remediation, Details: rds}
						ctta = append(ctta, ctt)
					}
					controlChecks = append(controlChecks, v1alpha1.ControlCheckDetails{ID: controlID,
						Name:               control.Name,
						Description:        control.Description,
						Severity:           control.Severity,
						ScannerCheckResult: ctta})
				}
			}
		}
	}
	return controlChecks
}

func (w *cm) checkIdsToResults(scannerResourceMap map[string]map[string]client.ObjectList) (map[string][]*ScannerCheckResult, error) {
	checkIdsToResults := make(map[string][]*ScannerCheckResult)
	for scanner, resourceListMap := range scannerResourceMap {
		for resourceName, resourceList := range resourceListMap {
			mapper, err := byScanner(scanner)
			if err != nil {
				return nil, err
			}
			idCheckResultMap := mapper.mapReportData(resourceName, resourceList)
			if idCheckResultMap == nil {
				continue
			}
			for id, scannerCheckResult := range idCheckResultMap {
				if _, ok := checkIdsToResults[id]; !ok {
					checkIdsToResults[id] = make([]*ScannerCheckResult, 0)
				}
				checkIdsToResults[id] = append(checkIdsToResults[id], scannerCheckResult)
			}
		}
	}
	return checkIdsToResults, nil
}

//populateSpecDataToMaps populate spec data to map structures
func (w *cm) populateSpecDataToMaps(spec v1alpha1.ReportSpec) *specDataMapping {
	//control to resource list map
	controlIDControlObject := make(map[string]v1alpha1.Control)
	//control to checks map
	controlCheckIds := make(map[string][]string)
	//scanner to resource list map
	scannerResourceListName := make(map[string]*hashset.Set)
	for _, control := range spec.Controls {
		control.Kinds = mapKinds(control)
		if _, ok := scannerResourceListName[control.Mapping.Scanner]; !ok {
			scannerResourceListName[control.Mapping.Scanner] = hashset.New()
		}
		for _, resource := range control.Kinds {
			scannerResourceListName[control.Mapping.Scanner].Add(resource)
		}
		controlIDControlObject[control.ID] = control
		//update control resource list map
		for _, check := range control.Mapping.Checks {
			if _, ok := controlCheckIds[control.ID]; !ok {
				controlCheckIds[control.ID] = make([]string, 0)
			}
			controlCheckIds[control.ID] = append(controlCheckIds[control.ID], check.ID)
		}
	}
	return &specDataMapping{
		scannerResourceListNames: scannerResourceListName,
		controlIDControlObject:   controlIDControlObject,
		controlCheckIds:          controlCheckIds}
}
