package stackitprovider

import (
	"context"
	"fmt"

	stackitdnsclient "github.com/stackitcloud/stackit-dns-api-client-go"
	"go.uber.org/zap"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// ApplyChanges applies a given set of changes in a given zone.
func (d *StackitDNSProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	// create rr set. POST /v1/projects/{projectId}/zones/{zoneId}/rrsets
	err := d.createRRSets(ctx, changes.Create)
	if err != nil {
		return err
	}

	// update rr set. PATCH /v1/projects/{projectId}/zones/{zoneId}/rrsets/{rrSetId}
	err = d.updateRRSets(ctx, changes.UpdateNew)
	if err != nil {
		return err
	}

	// delete rr set. DELETE /v1/projects/{projectId}/zones/{zoneId}/rrsets/{rrSetId}
	err = d.deleteRRSets(ctx, changes.Delete)
	if err != nil {
		return err
	}

	return nil
}

// createRRSets creates new record sets in the stackitprovider for the given endpoints that are in the
// creation field.
func (d *StackitDNSProvider) createRRSets(
	ctx context.Context,
	endpoints []*endpoint.Endpoint,
) error {
	if len(endpoints) == 0 {
		return nil
	}

	zones, err := d.zoneFetcherClient.zones(ctx)
	if err != nil {
		return err
	}

	return d.handleRRSetWithWorkers(ctx, endpoints, zones, CREATE)
}

// createRRSet creates a new record set in the stackitprovider for the given endpoint.
func (d *StackitDNSProvider) createRRSet(
	ctx context.Context,
	change *endpoint.Endpoint,
	zones []stackitdnsclient.DomainZone,
) error {
	resultZone, found := findBestMatchingZone(change.DNSName, zones)
	if !found {
		return fmt.Errorf("no matching zone found for %s", change.DNSName)
	}

	logFields := getLogFields(change, CREATE, resultZone.Id)
	d.logger.Info("create record set", logFields...)

	if d.dryRun {
		d.logger.Debug("dry run, skipping", logFields...)

		return nil
	}

	modifyChange(change)

	rrSet := getStackitRRSetRecordPost(change)

	// ignore all errors to just retry on next run
	_, _, err := d.apiClient.RecordSetApi.V1ProjectsProjectIdZonesZoneIdRrsetsPost(
		ctx,
		rrSet,
		d.projectId,
		resultZone.Id,
	)
	if err != nil {
		message := getSwaggerErrorMessage(err)
		d.logger.Error("error creating record set", zap.String("err", message))

		return err
	}

	d.logger.Info("create record set successfully", logFields...)

	return nil
}

// updateRRSets patches (overrides) contents in the record sets in the stackitprovider for the given
// endpoints that are in the update new field.
func (d *StackitDNSProvider) updateRRSets(
	ctx context.Context,
	endpoints []*endpoint.Endpoint,
) error {
	if len(endpoints) == 0 {
		return nil
	}

	zones, err := d.zoneFetcherClient.zones(ctx)
	if err != nil {
		return err
	}

	return d.handleRRSetWithWorkers(ctx, endpoints, zones, UPDATE)
}

// updateRRSet patches (overrides) contents in the record set in the stackitprovider.
func (d *StackitDNSProvider) updateRRSet(
	ctx context.Context,
	change *endpoint.Endpoint,
	zones []stackitdnsclient.DomainZone,
) error {
	modifyChange(change)

	resultZone, resultRRSet, err := d.rrSetFetcherClient.getRRSetForUpdateDeletion(ctx, change, zones)
	if err != nil {
		return err
	}

	logFields := getLogFields(change, UPDATE, resultRRSet.Id)
	d.logger.Info("update record set", logFields...)

	if d.dryRun {
		d.logger.Debug("dry run, skipping", logFields...)

		return nil
	}

	rrSet := getStackitRRSetRecordPatch(change)

	_, _, err = d.apiClient.RecordSetApi.V1ProjectsProjectIdZonesZoneIdRrsetsRrSetIdPatch(
		ctx,
		rrSet,
		d.projectId,
		resultZone.Id,
		resultRRSet.Id,
	)
	if err != nil {
		message := getSwaggerErrorMessage(err)
		d.logger.Error("error updating record set", zap.String("err", message))

		return err
	}

	d.logger.Info("update record set successfully", logFields...)

	return nil
}

// deleteRRSets delete record sets in the stackitprovider for the given endpoints that are in the
// deletion field.
func (d *StackitDNSProvider) deleteRRSets(
	ctx context.Context,
	endpoints []*endpoint.Endpoint,
) error {
	if len(endpoints) == 0 {
		d.logger.Debug("no endpoints to delete")

		return nil
	}

	d.logger.Info("records to delete", zap.String("records", fmt.Sprintf("%v", endpoints)))

	zones, err := d.zoneFetcherClient.zones(ctx)
	if err != nil {
		return err
	}

	return d.handleRRSetWithWorkers(ctx, endpoints, zones, DELETE)
}

// deleteRRSet deletes a record set in the stackitprovider for the given endpoint.
func (d *StackitDNSProvider) deleteRRSet(
	ctx context.Context,
	change *endpoint.Endpoint,
	zones []stackitdnsclient.DomainZone,
) error {
	modifyChange(change)

	resultZone, resultRRSet, err := d.rrSetFetcherClient.getRRSetForUpdateDeletion(ctx, change, zones)
	if err != nil {
		return err
	}

	logFields := getLogFields(change, DELETE, resultRRSet.Id)
	d.logger.Info("delete record set", logFields...)

	if d.dryRun {
		d.logger.Debug("dry run, skipping", logFields...)

		return nil
	}

	_, _, err = d.apiClient.RecordSetApi.V1ProjectsProjectIdZonesZoneIdRrsetsRrSetIdDelete(
		ctx,
		d.projectId,
		resultZone.Id,
		resultRRSet.Id,
	)
	if err != nil {
		message := getSwaggerErrorMessage(err)
		d.logger.Error("error deleting record set", zap.String("err", message))

		return err
	}

	d.logger.Info("delete record set successfully", logFields...)

	return nil
}

// handleRRSetWithWorkers handles the given endpoints with workers to optimize speed.
func (d *StackitDNSProvider) handleRRSetWithWorkers(
	ctx context.Context,
	endpoints []*endpoint.Endpoint,
	zones []stackitdnsclient.DomainZone,
	action string,
) error {
	workerChannel := make(chan changeTask, len(endpoints))
	errorChannel := make(chan error, len(endpoints))

	for i := 0; i < d.workers; i++ {
		go d.changeWorker(ctx, workerChannel, errorChannel, zones)
	}

	for _, change := range endpoints {
		workerChannel <- changeTask{
			action: action,
			change: change,
		}
	}

	for i := 0; i < len(endpoints); i++ {
		err := <-errorChannel
		if err != nil {
			close(workerChannel)

			return err
		}
	}

	close(workerChannel)

	return nil
}

// changeWorker is a worker that handles changes passed by a channel.
func (d *StackitDNSProvider) changeWorker(
	ctx context.Context,
	changes chan changeTask,
	errorChannel chan error,
	zones []stackitdnsclient.DomainZone,
) {
	for change := range changes {
		switch change.action {
		case CREATE:
			err := d.createRRSet(ctx, change.change, zones)
			errorChannel <- err
		case UPDATE:
			err := d.updateRRSet(ctx, change.change, zones)
			errorChannel <- err
		case DELETE:
			err := d.deleteRRSet(ctx, change.change, zones)
			errorChannel <- err
		}
	}

	d.logger.Debug("change worker finished")
}
