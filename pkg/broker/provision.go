package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/azure-service-broker/pkg/async/model"
	"github.com/Azure/azure-service-broker/pkg/service"
	log "github.com/Sirupsen/logrus"
)

func (b *broker) doProvisionStep(
	ctx context.Context,
	args map[string]string,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stepName, ok := args["stepName"]
	if !ok {
		return errors.New(`missing required argument "stepName"`)
	}
	instanceID, ok := args["instanceID"]
	if !ok {
		return errors.New(`missing required argument "instanceID"`)
	}
	instance, ok, err := b.store.GetInstance(instanceID)
	if err != nil {
		return b.handleProvisioningError(
			instanceID,
			stepName,
			err,
			"error loading persisted instance",
		)
	}
	if !ok {
		return b.handleProvisioningError(
			instanceID,
			stepName,
			nil,
			"instance does not exist in the data store",
		)
	}
	log.WithFields(log.Fields{
		"step":       stepName,
		"instanceID": instance.InstanceID,
	}).Debug("executing provisioning step")
	module, ok := b.modules[instance.ServiceID]
	if !ok {
		return b.handleProvisioningError(
			instance,
			stepName,
			nil,
			fmt.Sprintf(
				`no module was found for handling service "%s"`,
				instance.ServiceID,
			),
		)
	}
	provisioningContext := module.GetEmptyProvisioningContext()
	err = instance.GetProvisioningContext(provisioningContext, b.codec)
	if err != nil {
		return b.handleProvisioningError(
			instance,
			stepName,
			err,
			"error decoding provisioningContext from persisted instance",
		)
	}
	provisioningParams := module.GetEmptyProvisioningParameters()
	err = instance.GetProvisioningParameters(provisioningParams, b.codec)
	if err != nil {
		return b.handleProvisioningError(
			instance,
			stepName,
			err,
			"error decoding provisioningParameters from persisted instance",
		)
	}
	provisioner, err := module.GetProvisioner(instance.ServiceID, instance.PlanID)
	if err != nil {
		return b.handleProvisioningError(
			instance,
			stepName,
			err,
			fmt.Sprintf(
				`error retrieving provisioner for service "%s"`,
				instance.ServiceID,
			),
		)
	}
	step, ok := provisioner.GetStep(stepName)
	if !ok {
		return b.handleProvisioningError(
			instance,
			stepName,
			nil,
			`provisioner does not know how to process step "%s"`,
		)
	}
	updatedProvisioningContext, err := step.Execute(
		ctx,
		instanceID,
		instance.ServiceID,
		instance.PlanID,
		provisioningContext,
		provisioningParams,
	)
	if err != nil {
		return b.handleProvisioningError(
			instance,
			stepName,
			err,
			"error executing provisioning step",
		)
	}
	err = instance.SetProvisioningContext(updatedProvisioningContext, b.codec)
	if err != nil {
		return b.handleProvisioningError(
			instance,
			stepName,
			err,
			"error encoding modified provisioningContext",
		)
	}
	if nextStepName, ok := provisioner.GetNextStepName(step.GetName()); ok {
		if err = b.store.WriteInstance(instance); err != nil {
			return b.handleProvisioningError(
				instance,
				stepName,
				err,
				"error persisting instance",
			)
		}
		task := model.NewTask(
			"provisionStep",
			map[string]string{
				"stepName":   nextStepName,
				"instanceID": instanceID,
			},
		)
		if err = b.asyncEngine.SubmitTask(task); err != nil {
			return b.handleProvisioningError(
				instance,
				stepName,
				err,
				fmt.Sprintf(`error enqueing next step: "%s"`, nextStepName),
			)
		}
	} else {
		// No next step-- we're done provisioning!
		instance.Status = service.InstanceStateProvisioned
		if err = b.store.WriteInstance(instance); err != nil {
			return b.handleProvisioningError(
				instance,
				stepName,
				err,
				"error persisting instance",
			)
		}
	}
	return nil
}

// handleProvisioningError tries to handle async provisioning errors. If an
// instance is passed in, its status is updated and an attempt is made to
// persist the instance with updated status. If this fails, we have a very
// serious problem on our hands, so we log that failure and kill the process.
// Barring such a failure, a nicely formatted error is returned to be, in-turn,
// returned by the caller of this function. If an instanceID is passed in
// (instead of an instance), only error formatting is handled.
func (b *broker) handleProvisioningError(
	instanceOrInstanceID interface{},
	stepName string,
	e error,
	msg string,
) error {
	instance, ok := instanceOrInstanceID.(*service.Instance)
	if !ok {
		instanceID := instanceOrInstanceID
		if e == nil {
			return fmt.Errorf(
				`error executing provisioning step "%s" for instance "%s": %s`,
				stepName,
				instanceID,
				msg,
			)
		}
		return fmt.Errorf(
			`error executing provisioning step "%s" for instance "%s": %s: %s`,
			stepName,
			instanceID,
			msg,
			e,
		)
	}
	// If we get to here, we have an instance (not just an instanceID)
	instance.Status = service.InstanceStateProvisioningFailed
	var ret error
	if e == nil {
		ret = fmt.Errorf(
			`error executing provisioning step "%s" for instance "%s": %s`,
			stepName,
			instance.InstanceID,
			msg,
		)
	} else {
		ret = fmt.Errorf(
			`error executing provisioning step "%s" for instance "%s": %s: %s`,
			stepName,
			instance.InstanceID,
			msg,
			e,
		)
	}
	instance.StatusReason = ret.Error()
	if err := b.store.WriteInstance(instance); err != nil {
		log.WithFields(log.Fields{
			"instanceID":       instance.InstanceID,
			"status":           instance.Status,
			"originalError":    ret,
			"persistenceError": err,
		}).Fatal("error persisting instance with updated status")
	}
	return ret
}
