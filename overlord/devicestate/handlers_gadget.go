// -*- Mode: Go; indent-tabs-mode: t -*-
/*
 * Copyright (C) 2016-2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package devicestate

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/gadget"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
)

func makeRollbackDir(name string) (string, error) {
	rollbackDir := filepath.Join(dirs.SnapRollbackDir, name)

	if err := os.MkdirAll(rollbackDir, 0750); err != nil {
		return "", err
	}

	return rollbackDir, nil
}

func currentGadgetInfo(st *state.State, curDeviceCtx snapstate.DeviceContext) (*gadget.GadgetData, error) {
	currentInfo, err := snapstate.GadgetInfo(st, curDeviceCtx)
	if err != nil && err != state.ErrNoState {
		return nil, err
	}
	if currentInfo == nil {
		// no current yet
		return nil, nil
	}

	ci, err := gadgetDataFromInfo(currentInfo, curDeviceCtx.Model())
	if err != nil {
		return nil, fmt.Errorf("cannot read current gadget snap details: %v", err)
	}
	return ci, nil
}

func pendingGadgetInfo(snapsup *snapstate.SnapSetup, pendingDeviceCtx snapstate.DeviceContext) (*gadget.GadgetData, error) {
	info, err := snap.ReadInfo(snapsup.InstanceName(), snapsup.SideInfo)
	if err != nil {
		return nil, fmt.Errorf("cannot read candidate gadget snap details: %v", err)
	}

	gi, err := gadgetDataFromInfo(info, pendingDeviceCtx.Model())
	if err != nil {
		return nil, fmt.Errorf("cannot read candidate snap gadget metadata: %v", err)
	}
	return gi, nil
}

var (
	gadgetUpdate = gadget.Update
)

func (m *DeviceManager) doUpdateGadgetAssets(t *state.Task, _ *tomb.Tomb) error {
	if release.OnClassic {
		return fmt.Errorf("cannot run update gadget assets task on a classic system")
	}

	st := t.State()
	st.Lock()
	defer st.Unlock()

	snapsup, err := snapstate.TaskSnapSetup(t)
	if err != nil {
		return err
	}

	remodelCtx, err := DeviceCtx(st, t, nil)
	if err != nil {
		return err
	}
	isRemodel := remodelCtx.ForRemodeling()
	groundDeviceCtx := remodelCtx.GroundContext()

	model := groundDeviceCtx.Model()
	if isRemodel {
		model = remodelCtx.Model()
	}
	// be extra paranoid when checking we are installing the right gadget
	expectedGadgetSnap := model.Gadget()
	if snapsup.InstanceName() != expectedGadgetSnap {
		return fmt.Errorf("cannot apply gadget assets update from non-model gadget snap %q, expected %q snap",
			snapsup.InstanceName(), expectedGadgetSnap)
	}

	updateData, err := pendingGadgetInfo(snapsup, remodelCtx)
	if err != nil {
		return err
	}

	currentData, err := currentGadgetInfo(t.State(), groundDeviceCtx)
	if err != nil {
		return err
	}
	if currentData == nil {
		// no updates during first boot & seeding
		return nil
	}

	snapRollbackDir, err := makeRollbackDir(fmt.Sprintf("%v_%v", snapsup.InstanceName(), snapsup.SideInfo.Revision))
	if err != nil {
		return fmt.Errorf("cannot prepare update rollback directory: %v", err)
	}

	var updatePolicy gadget.UpdatePolicyFunc = nil

	if isRemodel {
		// use the remodel policy which triggers an update of all
		// structures
		updatePolicy = gadget.RemodelUpdatePolicy
	}

	var updateObserver gadget.ContentUpdateObserver
	observeTrustedBootAssets, err := boot.TrustedAssetsUpdateObserverForModel(model)
	if err != nil && err != boot.ErrObserverNotApplicable {
		return fmt.Errorf("cannot setup asset update observer: %v", err)
	}
	if err == nil {
		updateObserver = observeTrustedBootAssets
	}
	st.Unlock()
	err = gadgetUpdate(*currentData, *updateData, snapRollbackDir, updatePolicy, updateObserver)
	st.Lock()
	if err != nil {
		if err == gadget.ErrNoUpdate {
			// no update needed
			t.Logf("No gadget assets update needed")
			return nil
		}
		return err
	}

	t.SetStatus(state.DoneStatus)

	if err := os.RemoveAll(snapRollbackDir); err != nil && !os.IsNotExist(err) {
		logger.Noticef("failed to remove gadget update rollback directory %q: %v", snapRollbackDir, err)
	}

	// TODO: consider having the option to do this early via recovery in
	// core20, have fallback code as well there
	st.RequestRestart(state.RestartSystem)

	return nil
}
