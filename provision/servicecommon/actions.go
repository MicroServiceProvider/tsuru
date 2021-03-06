// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servicecommon

import (
	"sort"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/action"
	"github.com/tsuru/tsuru/app/image"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/set"
)

type ProcessState struct {
	Stop      bool
	Start     bool
	Restart   bool
	Sleep     bool
	Increment int
}

type ProcessSpec map[string]ProcessState

type pipelineArgs struct {
	manager          ServiceManager
	app              provision.App
	newImage         string
	newImageSpec     ProcessSpec
	currentImage     string
	currentImageSpec ProcessSpec
}

type ServiceManager interface {
	RemoveService(a provision.App, processName string) error
	CurrentLabels(a provision.App, processName string) (*provision.LabelSet, error)
	DeployService(a provision.App, processName string, labels *provision.LabelSet, replicas int, image string) error
}

func RunServicePipeline(manager ServiceManager, a provision.App, newImg string, updateSpec ProcessSpec) error {
	curImg, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	currentImageData, err := image.GetImageCustomData(curImg)
	if err != nil {
		return err
	}
	currentSpec := ProcessSpec{}
	for p := range currentImageData.Processes {
		currentSpec[p] = ProcessState{}
	}
	newImageData, err := image.GetImageCustomData(newImg)
	if err != nil {
		return err
	}
	if len(newImageData.Processes) == 0 {
		return errors.Errorf("no process information found deploying image %q", newImg)
	}
	newSpec := ProcessSpec{}
	for p := range newImageData.Processes {
		newSpec[p] = ProcessState{Start: true}
		if updateSpec != nil {
			newSpec[p] = updateSpec[p]
		}
	}
	pipeline := action.NewPipeline(
		updateServices,
		updateImageInDB,
		removeOldServices,
	)
	return pipeline.Execute(&pipelineArgs{
		manager:          manager,
		app:              a,
		newImage:         newImg,
		newImageSpec:     newSpec,
		currentImage:     curImg,
		currentImageSpec: currentSpec,
	})
}

func rollbackAddedProcesses(args *pipelineArgs, processes []string) {
	for _, processName := range processes {
		var err error
		if state, in := args.currentImageSpec[processName]; in {
			err = deployService(args, processName, args.currentImage, state)
		} else {
			err = args.manager.RemoveService(args.app, processName)
		}
		if err != nil {
			log.Errorf("error rolling back updated service for %s[%s]: %+v", args.app.GetName(), processName, err)
		}
	}
}

func deployService(args *pipelineArgs, processName, image string, pState ProcessState) error {
	oldLabels, err := args.manager.CurrentLabels(args.app, processName)
	if err != nil {
		return err
	}
	replicas := 0
	restartCount := 0
	isStopped := false
	isAsleep := false
	if oldLabels != nil {
		replicas = oldLabels.AppReplicas()
		restartCount = oldLabels.Restarts()
		isStopped = oldLabels.IsStopped()
		isAsleep = oldLabels.IsAsleep()
	}
	if pState.Increment != 0 {
		replicas += pState.Increment
		if replicas < 0 {
			return errors.New("cannot have less than 0 units")
		}
	}
	if pState.Start || pState.Restart {
		if replicas == 0 {
			replicas = 1
		}
		isStopped = false
		isAsleep = false
	}
	labels, err := provision.ServiceLabels(provision.ServiceLabelsOpts{
		App:      args.app,
		Process:  processName,
		Replicas: replicas,
	})
	if err != nil {
		return err
	}
	realReplicas := replicas
	if isStopped || pState.Stop {
		realReplicas = 0
		labels.SetStopped()
	}
	if isAsleep || pState.Sleep {
		labels.SetAsleep()
	}
	if pState.Restart {
		restartCount++
		labels.SetRestarts(restartCount)
	}
	return args.manager.DeployService(args.app, processName, labels, realReplicas, image)
}

var updateServices = &action.Action{
	Name: "update-services",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		args := ctx.Params[0].(*pipelineArgs)
		var (
			toDeployProcesses []string
			deployedProcesses []string
			err               error
		)
		for processName := range args.newImageSpec {
			toDeployProcesses = append(toDeployProcesses, processName)
		}
		sort.Strings(toDeployProcesses)
		for _, processName := range toDeployProcesses {
			err = deployService(args, processName, args.newImage, args.newImageSpec[processName])
			if err != nil {
				break
			}
			deployedProcesses = append(deployedProcesses, processName)
		}
		if err != nil {
			rollbackAddedProcesses(args, deployedProcesses)
			return nil, err
		}
		return deployedProcesses, nil
	},
	Backward: func(ctx action.BWContext) {
		args := ctx.Params[0].(*pipelineArgs)
		deployedProcesses := ctx.FWResult.([]string)
		rollbackAddedProcesses(args, deployedProcesses)
	},
}

var updateImageInDB = &action.Action{
	Name: "update-image-in-db",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		args := ctx.Params[0].(*pipelineArgs)
		err := image.AppendAppImageName(args.app.GetName(), args.newImage)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return ctx.Previous, nil
	},
}

var removeOldServices = &action.Action{
	Name: "remove-old-services",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		args := ctx.Params[0].(*pipelineArgs)
		old := set.FromMap(args.currentImageSpec)
		new := set.FromMap(args.newImageSpec)
		for processName := range old.Difference(new) {
			err := args.manager.RemoveService(args.app, processName)
			if err != nil {
				log.Errorf("ignored error removing unwanted service for %s[%s]: %+v", args.app.GetName(), processName, err)
			}
		}
		return nil, nil
	},
}
