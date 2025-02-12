package scandataexport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gocarina/gocsv"
	"github.com/opencontainers/go-digest"

	"github.com/goharbor/harbor/src/jobservice/job"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/pkg/project"
	"github.com/goharbor/harbor/src/pkg/scan/export"
	"github.com/goharbor/harbor/src/pkg/systemartifact"
	"github.com/goharbor/harbor/src/pkg/systemartifact/model"
	"github.com/goharbor/harbor/src/pkg/task"
)

// ScanDataExport is the struct to implement the scan data export.
// implements the Job interface
type ScanDataExport struct {
	execMgr               task.ExecutionManager
	scanDataExportDirPath string
	exportMgr             export.Manager
	digestCalculator      export.ArtifactDigestCalculator
	filterProcessor       export.FilterProcessor
	vulnDataSelector      export.VulnerabilityDataSelector
	projectMgr            project.Manager
	sysArtifactMgr        systemartifact.Manager
}

func (sde *ScanDataExport) MaxFails() uint {
	return 1
}

// MaxCurrency of the job. Unlike the WorkerPool concurrency, it controls the limit on the number jobs of that type
// that can be active at one time by within a single redis instance.
// The default value is 0, which means "no limit on job concurrency".
func (sde *ScanDataExport) MaxCurrency() uint {
	return 1
}

// ShouldRetry tells worker if retry the failed job when the fails is
// still less that the number declared by the method 'MaxFails'.
//
// Returns:
//  true for retry and false for none-retry
func (sde *ScanDataExport) ShouldRetry() bool {
	return true
}

// Validate Indicate whether the parameters of job are valid.
// Return:
// error if parameters are not valid. NOTES: If no parameters needed, directly return nil.
func (sde *ScanDataExport) Validate(params job.Parameters) error {
	return nil
}

// Run the business logic here.
// The related arguments will be injected by the workerpool.
//
// ctx Context                   : Job execution context.
// params map[string]interface{} : parameters with key-pair style for the job execution.
//
// Returns:
//  error if failed to run. NOTES: If job is stopped or cancelled, a specified error should be returned
//
func (sde *ScanDataExport) Run(ctx job.Context, params job.Parameters) error {
	if _, ok := params[export.JobModeKey]; !ok {
		return errors.Errorf("no mode specified for scan data export execution")
	}

	mode := params[export.JobModeKey].(string)
	logger := ctx.GetLogger()
	logger.Infof("Scan data export job started in mode : %v", mode)
	sde.init()
	fileName := fmt.Sprintf("%s/scandata_export_%v.csv", sde.scanDataExportDirPath, params["JobId"])

	// ensure that CSV files are cleared post the completion of the Run.
	defer sde.cleanupCsvFile(ctx, fileName, params)
	err := sde.writeCsvFile(ctx, params, fileName)
	if err != nil {
		logger.Errorf("error when writing data to CSV: %v", err)
		return err
	}

	hash, err := sde.calculateFileHash(fileName)
	if err != nil {
		logger.Errorf("Error when calculating checksum for generated file: %v", err)
		return err
	}
	logger.Infof("Export Job Id = %v, FileName = %s, Hash = %v", params["JobId"], fileName, hash)

	csvFile, err := os.OpenFile(fileName, os.O_RDONLY, os.ModePerm)
	if err != nil {
		logger.Errorf(
			"Export Job Id = %v. Error when moving report file %s to persistent storage: %v", params["JobId"], fileName, err)
		return err
	}
	baseFileName := filepath.Base(fileName)
	repositoryName := strings.TrimSuffix(baseFileName, filepath.Ext(baseFileName))
	logger.Infof("Creating repository for CSV file with blob : %s", repositoryName)
	stat, err := os.Stat(fileName)
	if err != nil {
		logger.Errorf("Error when fetching file size: %v", err)
		return err
	}
	logger.Infof("Export Job Id = %v. CSV file size: %d", params["JobId"], stat.Size())
	csvExportArtifactRecord := model.SystemArtifact{Repository: repositoryName, Digest: hash.String(), Size: stat.Size(), Type: "ScanData_CSV", Vendor: strings.ToLower(export.Vendor)}
	artID, err := sde.sysArtifactMgr.Create(ctx.SystemContext(), &csvExportArtifactRecord, csvFile)
	if err != nil {
		logger.Errorf(
			"Export Job Id = %v. Error when persisting report file %s to persistent storage: %v", params["JobId"], fileName, err)
		return err
	}

	logger.Infof("Export Job Id = %v. Created system artifact: %v for report file %s to persistent storage: %v", params["JobId"], artID, fileName, err)
	err = sde.updateExecAttributes(ctx, params, err, hash)

	if err != nil {
		logger.Errorf("Export Job Id = %v. Error when updating execution record : %v", params["JobId"], err)
		return err
	}
	logger.Info("Scan data export job completed")

	return nil
}

func (sde *ScanDataExport) updateExecAttributes(ctx job.Context, params job.Parameters, err error, hash digest.Digest) error {
	execID := int64(params["JobId"].(float64))
	exec, err := sde.execMgr.Get(ctx.SystemContext(), execID)
	logger := ctx.GetLogger()
	if err != nil {
		logger.Errorf("Export Job Id = %v. Error when fetching execution record for update : %v", params["JobId"], err)
		return err
	}
	attrsToUpdate := make(map[string]interface{})
	for k, v := range exec.ExtraAttrs {
		attrsToUpdate[k] = v
	}
	attrsToUpdate[export.DigestKey] = hash.String()
	return sde.execMgr.UpdateExtraAttrs(ctx.SystemContext(), execID, attrsToUpdate)
}

func (sde *ScanDataExport) writeCsvFile(ctx job.Context, params job.Parameters, fileName string) error {
	csvFile, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, os.ModePerm)
	if err != nil {
		return err
	}
	systemContext := ctx.SystemContext()
	defer csvFile.Close()

	logger := ctx.GetLogger()
	if err != nil {
		logger.Errorf("Failed to create CSV export file %s. Error : %v", fileName, err)
		return err
	}
	logger.Infof("Created CSV export file %s", csvFile.Name())

	var exportParams export.Params
	var artIDGroups [][]int64

	if criteira, ok := params["Request"]; ok {
		logger.Infof("Request for export : %v", criteira)
		filterCriteria, err := sde.extractCriteria(params)
		if err != nil {
			return err
		}

		// check if any projects are specified. If not then fetch all the projects
		// of which the current user is a project admin.
		projectIds, err := sde.filterProcessor.ProcessProjectFilter(systemContext, filterCriteria.UserName, filterCriteria.Projects)

		if err != nil {
			return err
		}

		if len(projectIds) == 0 {
			return nil
		}

		// extract the repository ids if any repositories have been specified
		repoIds, err := sde.filterProcessor.ProcessRepositoryFilter(systemContext, filterCriteria.Repositories, projectIds)
		if err != nil {
			return err
		}

		if len(repoIds) == 0 {
			logger.Infof("No repositories found with specified names: %v", filterCriteria.Repositories)
			return nil
		}

		// filter artifacts by tags
		arts, err := sde.filterProcessor.ProcessTagFilter(systemContext, filterCriteria.Tags, repoIds)
		if err != nil {
			return err
		}

		if len(arts) == 0 {
			logger.Infof("No artifacts found with specified names: %v and tags: %v", filterCriteria.Repositories, filterCriteria.Tags)
			return nil
		}

		// filter artifacts by labels
		arts, err = sde.filterProcessor.ProcessLabelFilter(systemContext, filterCriteria.Labels, arts)
		if err != nil {
			return err
		}

		if len(arts) == 0 {
			logger.Infof("No artifacts found with specified labels: %v", filterCriteria.Labels)
			return nil
		}

		size := export.ArtifactGroupSize
		artIDGroups = make([][]int64, len(arts)/size+1)
		for i, art := range arts {
			// group artIDs to improve performance and avoid spliced sql over
			// max length
			artIDGroups[i/size] = append(artIDGroups[i/size], art.ID)
		}

		exportParams = export.Params{
			CVEIds: filterCriteria.CVEIds,
		}
	}

	for groupID, artIDGroup := range artIDGroups {
		// fetch data by group
		if len(artIDGroup) == 0 {
			continue
		}

		exportParams.ArtifactIDs = artIDGroup
		exportParams.PageNumber = 1
		exportParams.PageSize = export.QueryPageSize

		for {
			data, err := sde.exportMgr.Fetch(systemContext, exportParams)
			if err != nil {
				logger.Error("Encountered error reading from the report table", err)
				return err
			}
			if len(data) == 0 {
				logger.Infof("No more data to fetch. Exiting...")
				break
			}
			logger.Infof("Export Group Id = %d, Job Id = %v, Page Number = %d, Page Size = %d Num Records = %d", groupID, params["JobId"], exportParams.PageNumber, exportParams.PageSize, len(data))

			// for the first page write the CSV with the headers
			if exportParams.PageNumber == 1 && groupID == 0 {
				err = gocsv.Marshal(data, csvFile)
			} else {
				err = gocsv.MarshalWithoutHeaders(data, csvFile)
			}
			if err != nil {
				return nil
			}

			exportParams.PageNumber = exportParams.PageNumber + 1
			exportParams.RowNumOffset = exportParams.RowNumOffset + int64(len(data))

			// break earlier if this is last page
			if len(data) < int(exportParams.PageSize) {
				break
			}
		}
	}
	return nil
}

func (sde *ScanDataExport) extractCriteria(params job.Parameters) (*export.Request, error) {
	filterMap, ok := params["Request"].(map[string]interface{})
	if !ok {
		return nil, errors.Errorf("malformed criteria '%v'", params["Request"])
	}
	jsonData, err := json.Marshal(filterMap)
	if err != nil {
		return nil, err
	}
	criteria := &export.Request{}
	err = criteria.FromJSON(string(jsonData))

	if err != nil {
		return nil, err
	}
	return criteria, nil
}

func (sde *ScanDataExport) calculateFileHash(fileName string) (digest.Digest, error) {
	return sde.digestCalculator.Calculate(fileName)
}

func (sde *ScanDataExport) init() {
	if sde.execMgr == nil {
		sde.execMgr = task.NewExecutionManager()
	}

	if sde.scanDataExportDirPath == "" {
		sde.scanDataExportDirPath = export.ScanDataExportDir
	}

	if sde.exportMgr == nil {
		sde.exportMgr = export.NewManager()
	}

	if sde.digestCalculator == nil {
		sde.digestCalculator = &export.SHA256ArtifactDigestCalculator{}
	}

	if sde.filterProcessor == nil {
		sde.filterProcessor = export.NewFilterProcessor()
	}

	if sde.vulnDataSelector == nil {
		sde.vulnDataSelector = export.NewVulnerabilityDataSelector()
	}

	if sde.projectMgr == nil {
		sde.projectMgr = project.New()
	}

	if sde.sysArtifactMgr == nil {
		sde.sysArtifactMgr = systemartifact.Mgr
	}
}

func (sde *ScanDataExport) cleanupCsvFile(ctx job.Context, fileName string, params job.Parameters) {
	logger := ctx.GetLogger()
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		logger.Infof("Export Job Id = %v, CSV Export File = %s does not exist. Nothing to do", params["JobId"], fileName)
		return
	}
	err := os.Remove(fileName)
	if err != nil {
		logger.Errorf("Export Job Id = %d, CSV Export File = %s could not deleted. Error = %v", params["JobId"], fileName, err)
		return
	}
}
