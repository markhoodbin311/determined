package db

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// ExperimentLabelUsage returns a flattened and deduplicated list of all the
// labels in use across all experiments.
func (db *PgDB) ExperimentLabelUsage() (labelUsage map[string]int, err error) {
	// First, assemble all the JSON lists that the database returns into a
	// single tally of all the labels
	type dbLabelList struct {
		Labels []byte
	}
	var rawLists []dbLabelList
	err = db.Query("get_experiment_labels", &rawLists)
	if err != nil {
		return nil, fmt.Errorf("error in get_experiment_labels query: %w", err)
	}
	labelUsage = make(map[string]int)
	for _, rawList := range rawLists {
		if len(rawList.Labels) == 0 {
			continue
		}
		var parsedList []string
		err = json.Unmarshal(rawList.Labels, &parsedList)
		if err != nil {
			return nil, fmt.Errorf("error parsing experiment labels: %w", err)
		}
		for i := range parsedList {
			label := parsedList[i]
			labelUsage[label]++
		}
	}
	return labelUsage, nil
}

// MetricNames returns the set of training and validation metric names that have been recorded for
// an experiment.
func (db *PgDB) MetricNames(experimentID int, sStartTime time.Time, vStartTime time.Time) (
	training []string, validation []string, sEndTime time.Time, vEndTime time.Time, err error) {
	type namesWrapper struct {
		Name    string    `db:"name"`
		EndTime time.Time `db:"end_time"`
	}

	var rows []namesWrapper
	err = db.queryRows(`
SELECT 
  jsonb_object_keys(s.metrics->'avg_metrics') AS name,
  max(s.end_time) AS end_time
FROM trials t
INNER JOIN steps s ON t.id=s.trial_id
WHERE t.experiment_id=$1
  AND s.end_time > $2
GROUP BY name;`, &rows, experimentID, sStartTime)
	if err != nil {
		return nil, nil, sEndTime, vEndTime, errors.Wrapf(err,
			"error querying training metric names for experiment %d", experimentID)
	}
	for _, row := range rows {
		training = append(training, row.Name)
		if row.EndTime.After(sEndTime) {
			sEndTime = row.EndTime
		}
	}

	err = db.queryRows(`
SELECT
  jsonb_object_keys(v.metrics->'validation_metrics') AS name,
  max(v.end_time) AS end_time
FROM trials t
INNER JOIN steps s ON t.id=s.trial_id
LEFT OUTER JOIN validations v ON s.id=v.step_id AND s.trial_id=v.trial_id
WHERE t.experiment_id=$1
  AND v.end_time > $2
GROUP BY name;`, &rows, experimentID, vStartTime)
	if err != nil {
		return nil, nil, sEndTime, vEndTime, errors.Wrapf(err,
			"error querying validation metric names for experiment %d", experimentID)
	}
	for _, row := range rows {
		validation = append(validation, row.Name)
		if row.EndTime.After(sEndTime) {
			sEndTime = row.EndTime
		}
	}

	return training, validation, sEndTime, vEndTime, err
}

type batchesWrapper struct {
	Batches int32     `db:"batches_processed"`
	EndTime time.Time `db:"end_time"`
}

// TrainingMetricBatches returns the milestones (in batches processed) at which a specific training
// metric was recorded.
func (db *PgDB) TrainingMetricBatches(experimentID int, metricName string, startTime time.Time) (
	batches []int32, endTime time.Time, err error) {
	var rows []*batchesWrapper
	err = db.queryRows(`
SELECT (s.prior_batches_processed + num_batches) AS batches_processed,
  max(s.end_time) as end_time
FROM trials t INNER JOIN steps s ON t.id=s.trial_id
WHERE t.experiment_id=$1
  AND s.state = 'COMPLETED'
  AND s.metrics->'avg_metrics' ? $2
  AND s.end_time > $3
GROUP BY batches_processed;`, &rows, experimentID, metricName, startTime)
	if err != nil {
		return nil, endTime, errors.Wrapf(err, "error querying DB for training metric batches")
	}
	for _, row := range rows {
		batches = append(batches, row.Batches)
		if row.EndTime.After(endTime) {
			endTime = row.EndTime
		}
	}

	return batches, endTime, nil
}

// ValidationMetricBatches returns the milestones (in batches processed) at which a specific
// validation metric was recorded.
func (db *PgDB) ValidationMetricBatches(experimentID int, metricName string, startTime time.Time) (
	batches []int32, endTime time.Time, err error) {
	var rows []*batchesWrapper
	err = db.queryRows(`
SELECT (s.prior_batches_processed + num_batches) AS batches_processed,
  max(v.end_time) as end_time
FROM trials t INNER JOIN steps s ON t.id=s.trial_id
  LEFT OUTER JOIN validations v ON s.id=v.step_id AND s.trial_id=v.trial_id
WHERE t.experiment_id=$1
  AND v.state = 'COMPLETED'
  AND v.metrics->'validation_metrics' ? $2
  AND v.end_time > $3
GROUP BY batches_processed;`, &rows, experimentID, metricName, startTime)
	if err != nil {
		return nil, endTime, errors.Wrapf(err, "error querying DB for validation metric batches")
	}
	for _, row := range rows {
		batches = append(batches, row.Batches)
		if row.EndTime.After(endTime) {
			endTime = row.EndTime
		}
	}

	return batches, endTime, nil
}

type snapshotWrapper struct {
	TrialID int32     `db:"trial_id"`
	Hparams []byte    `db:"hparams"`
	Metric  float64   `db:"metric"`
	EndTime time.Time `db:"end_time"`
}

func snapshotWrapperToTrial(r snapshotWrapper) (*apiv1.TrialsSnapshotResponse_Trial, error) {
	var trial apiv1.TrialsSnapshotResponse_Trial
	trial.TrialId = r.TrialID

	var inter map[string]interface{}
	err := json.Unmarshal(r.Hparams, &inter)
	if err != nil {
		return nil, err
	}
	trial.Hparams = protoutils.ToStruct(inter)
	trial.Metric = r.Metric
	return &trial, nil
}

// TrainingTrialsSnapshot returns a training metric across each trial in an experiment at a
// specific point of progress.
func (db *PgDB) TrainingTrialsSnapshot(experimentID int, batchesProcessed int, metricName string,
	startTime time.Time) (trials []*apiv1.TrialsSnapshotResponse_Trial, endTime time.Time,
	err error) {
	var rows []snapshotWrapper
	err = db.queryRows(`
SELECT
  t.id AS trial_id,
  t.hparams AS hparams,
  s.metrics->'avg_metrics'->$1 AS metric,
  s.end_time AS end_time
FROM trials t
  INNER JOIN steps s ON t.id=s.trial_id
WHERE t.experiment_id=$2
  AND (s.prior_batches_processed + num_batches)=$3
  AND s.end_time > $4
ORDER BY s.end_time;`, &rows, metricName, experimentID, batchesProcessed, startTime)
	if err != nil {
		return nil, endTime, errors.Wrapf(err,
			"failed to get snapshot for experiment %d and training metric %s",
			experimentID, metricName)
	}
	for _, row := range rows {
		trial, err := snapshotWrapperToTrial(row)
		if err != nil {
			return nil, endTime, errors.Wrap(err, "Failed to process trial metadata")
		}
		trials = append(trials, trial)
		if row.EndTime.After(endTime) {
			endTime = row.EndTime
		}
	}

	return trials, endTime, nil
}

// ValidationTrialsSnapshot returns a training metric across each trial in an experiment at a
// specific point of progress.
func (db *PgDB) ValidationTrialsSnapshot(experimentID int, batchesProcessed int, metricName string,
	startTime time.Time) (trials []*apiv1.TrialsSnapshotResponse_Trial, endTime time.Time,
	err error) {
	var rows []snapshotWrapper
	err = db.queryRows(`
SELECT
  t.id AS trial_id,
  t.hparams AS hparams,
  v.metrics->'validation_metrics'->$1 AS metric,
  v.end_time AS end_time
FROM trials t
  INNER JOIN steps s ON t.id=s.trial_id
  LEFT OUTER JOIN validations v ON s.id=v.step_id AND s.trial_id=v.trial_id
WHERE t.experiment_id=$2
  AND (s.prior_batches_processed + num_batches)=$3
  AND v.end_time > $4
ORDER BY v.end_time;`, &rows, metricName, experimentID, batchesProcessed, startTime)
	if err != nil {
		return nil, endTime, errors.Wrapf(err,
			"failed to get snapshot for experiment %d and validation metric %s",
			experimentID, metricName)
	}

	for _, row := range rows {
		trial, err := snapshotWrapperToTrial(row)
		if err != nil {
			return nil, endTime, errors.Wrap(err, "Failed to process trial metadata")
		}
		trials = append(trials, trial)
		if row.EndTime.After(endTime) {
			endTime = row.EndTime
		}
	}

	return trials, endTime, nil
}
