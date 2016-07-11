package localdriver

import (
	"errors"
	"fmt"
	"os"

	"strings"

	"code.cloudfoundry.org/lager"
	"github.com/cloudfoundry-incubator/voldriver"
	"path/filepath"
)

const VolumesRootDir = "_volumes"
const MountsRootDir = "_mounts"

type LocalDriver struct { // see voldriver.resources.go
	volumes       map[string]*voldriver.VolumeInfo
	fileSystem    FileSystem
	invoker       Invoker
	mountPathRoot string
}

func NewLocalDriver(fileSystem FileSystem, invoker Invoker, mountPathRoot string) *LocalDriver {
	return &LocalDriver{
		volumes:       map[string]*voldriver.VolumeInfo{},
		fileSystem:    fileSystem,
		invoker:       invoker,
		mountPathRoot: mountPathRoot,
	}
}

func (d *LocalDriver) Activate(logger lager.Logger) voldriver.ActivateResponse {
	return voldriver.ActivateResponse{
		Implements: []string{"VolumeDriver"},
	}
}

func (d *LocalDriver) Create(logger lager.Logger, createRequest voldriver.CreateRequest) voldriver.ErrorResponse {
	logger = logger.Session("create")
	var ok bool
	var id interface{}

	if createRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	if id, ok = createRequest.Opts["volume_id"]; !ok {
		logger.Info("missing-volume-id", lager.Data{"volume_name": createRequest.Name})
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_id' field in 'Opts'"}
	}

	var existingVolume *voldriver.VolumeInfo
	if existingVolume, ok = d.volumes[createRequest.Name]; !ok {
		logger.Info("creating-volume", lager.Data{"volume_name": createRequest.Name, "volume_id": id.(string)})
		d.volumes[createRequest.Name] = &voldriver.VolumeInfo{Name: id.(string)}

		createDir := d.volumePath(logger, id.(string))
		logger.Info("creating-volume-folder", lager.Data{"volume": createDir})
		os.MkdirAll(createDir, os.ModePerm)

		return voldriver.ErrorResponse{}
	}

	// If a volume with the given name already exists, no-op unless the opts are different
	if existingVolume.Name != id {
		logger.Info("duplicate-volume", lager.Data{"volume_name": createRequest.Name})
		return voldriver.ErrorResponse{Err: fmt.Sprintf("Volume '%s' already exists with a different volume ID", createRequest.Name)}
	}

	return voldriver.ErrorResponse{}
}

func (d *LocalDriver) List(logger lager.Logger) voldriver.ListResponse {
	listResponse := voldriver.ListResponse{}
	for _, volume := range d.volumes {
		listResponse.Volumes = append(listResponse.Volumes, *volume)
	}
	listResponse.Err = ""
	return listResponse
}

func (d *LocalDriver) Mount(logger lager.Logger, mountRequest voldriver.MountRequest) voldriver.MountResponse {
	logger = logger.Session("mount", lager.Data{"volume": mountRequest.Name})

	if mountRequest.Name == "" {
		return voldriver.MountResponse{Err: "Missing mandatory 'volume_name'"}
	}

	var vol *voldriver.VolumeInfo
	var ok bool
	if vol, ok = d.volumes[mountRequest.Name]; !ok {
		return voldriver.MountResponse{Err: fmt.Sprintf("Volume '%s' must be created before being mounted", mountRequest.Name)}
	}

	volumePath := d.volumePath(logger, vol.Name)

	mountPath := d.mountPath(logger, vol.Name)
	logger.Info("mounting-volume", lager.Data{"id": vol.Name, "mountpoint": mountPath})

	err := d.mount(logger, volumePath, mountPath)
	if err != nil {
		logger.Error("mount-volume-failed", err)
		return voldriver.MountResponse{Err: fmt.Sprintf("Error mounting volume: %s", err.Error())}
	}

	vol.Mountpoint = mountPath

	vol.MountCount++
	logger.Info("volume-mounted", lager.Data{"name": vol.Name, "count": vol.MountCount})

	mountResponse := voldriver.MountResponse{Mountpoint: mountPath}
	return mountResponse
}

func (d *LocalDriver) Path(logger lager.Logger, pathRequest voldriver.PathRequest) voldriver.PathResponse {
	logger = logger.Session("path", lager.Data{"volume": pathRequest.Name})

	if pathRequest.Name == "" {
		return voldriver.PathResponse{Err: "Missing mandatory 'volume_name'"}
	}

	mountPath, err := d.get(logger, pathRequest.Name)
	if err != nil {
		logger.Error("failed-no-such-volume-found", err, lager.Data{"mountpoint": mountPath})

		return voldriver.PathResponse{Err: fmt.Sprintf("Volume '%s' not found", pathRequest.Name)}
	}

	if mountPath == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.PathResponse{Err: errText}
	}

	return voldriver.PathResponse{Mountpoint: mountPath}
}

func (d *LocalDriver) Unmount(logger lager.Logger, unmountRequest voldriver.UnmountRequest) voldriver.ErrorResponse {
	logger = logger.Session("unmount", lager.Data{"volume": unmountRequest.Name})

	if unmountRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	mountPath, err := d.get(logger, unmountRequest.Name)
	if err != nil {
		logger.Error("failed-no-such-volume-found", err, lager.Data{"mountpoint": mountPath})

		return voldriver.ErrorResponse{Err: fmt.Sprintf("Volume '%s' not found", unmountRequest.Name)}
	}

	if mountPath == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.ErrorResponse{Err: errText}
	}

	return d.unmount(logger, unmountRequest.Name, mountPath)
}

func (d *LocalDriver) Remove(logger lager.Logger, removeRequest voldriver.RemoveRequest) voldriver.ErrorResponse {
	logger = logger.Session("remove", lager.Data{"volume": removeRequest})
	logger.Info("start")
	defer logger.Info("end")

	if removeRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	var response voldriver.ErrorResponse
	var vol *voldriver.VolumeInfo
	var exists bool
	if vol, exists = d.volumes[removeRequest.Name]; !exists {
		logger.Error("failed-volume-removal", fmt.Errorf(fmt.Sprintf("Volume %s not found", removeRequest.Name)))
		return voldriver.ErrorResponse{fmt.Sprintf("Volume '%s' not found", removeRequest.Name)}
	}

	if vol.Mountpoint != "" {
		response = d.unmount(logger, removeRequest.Name, vol.Mountpoint)
		if response.Err != "" {
			return response
		}
	}

	mountPath := d.volumePath(logger, vol.Name)

	logger.Info("remove-volume-folder", lager.Data{"volume": mountPath})
	err := d.fileSystem.RemoveAll(mountPath)
	if err != nil {
		logger.Error("failed-removing-volume", err)
		return voldriver.ErrorResponse{Err: fmt.Sprintf("Failed removing mount path: %s", err)}
	}

	logger.Info("removing-volume", lager.Data{"name": removeRequest.Name})
	delete(d.volumes, removeRequest.Name)
	return voldriver.ErrorResponse{}
}

func (d *LocalDriver) Get(logger lager.Logger, getRequest voldriver.GetRequest) voldriver.GetResponse {
	mountpoint, err := d.get(logger, getRequest.Name)
	if err != nil {
		return voldriver.GetResponse{Err: err.Error()}
	}

	return voldriver.GetResponse{Volume: voldriver.VolumeInfo{Name: getRequest.Name, Mountpoint: mountpoint}}
}

func (d *LocalDriver) get(logger lager.Logger, volumeName string) (string, error) {
	if vol, ok := d.volumes[volumeName]; ok {
		logger.Info("getting-volume", lager.Data{"name": volumeName})
		return vol.Mountpoint, nil
	}

	return "", errors.New("Volume not found")
}

func (d *LocalDriver) exists(path string) (bool, error) {
	_, err := d.fileSystem.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func (d *LocalDriver) mountPath(logger lager.Logger, volumeId string) string {
	dir, err := d.fileSystem.Abs(d.mountPathRoot)
	if err != nil {
		logger.Fatal("abs-failed", err)
	}

	if !strings.HasSuffix(dir, "/") {
		dir = fmt.Sprintf("%s/", dir)
	}

	mountsPathRoot := fmt.Sprintf("%s%s", dir, MountsRootDir)
	d.fileSystem.MkdirAll(mountsPathRoot, os.ModePerm)

	return fmt.Sprintf("%s/%s", mountsPathRoot, volumeId)
}

func (d *LocalDriver) volumePath(logger lager.Logger, volumeId string) string {
	dir, err := d.fileSystem.Abs(d.mountPathRoot)
	if err != nil {
		logger.Fatal("abs-failed", err)
	}

	volumesPathRoot := filepath.Join(dir, VolumesRootDir)
	d.fileSystem.MkdirAll(volumesPathRoot, os.ModePerm)

	return filepath.Join(volumesPathRoot, volumeId)
}

func (d *LocalDriver) mount(logger lager.Logger, volumePath, mountPath string) error {
	logger.Info("link", lager.Data{"src": volumePath, "tgt": mountPath})
	args := []string{"-s", volumePath, mountPath}
	return d.invoker.Invoke(logger, "ln", args)
}

func (d *LocalDriver) unmount(logger lager.Logger, name string, mountPath string) voldriver.ErrorResponse {
	logger = logger.Session("unmount")
	logger.Info("start")
	defer logger.Info("end")

	exists, err := d.exists(mountPath)
	if err != nil {
		logger.Error("failed-retrieving-mount-info", err, lager.Data{"mountpoint": mountPath})
		return voldriver.ErrorResponse{Err: "Error establishing whether volume exists"}
	}

	if !exists {
		errText := fmt.Sprintf("Volume %s does not exist (path: %s), nothing to do!", name, mountPath)
		logger.Error("failed-mountpoint-not-found", errors.New(errText))
		return voldriver.ErrorResponse{Err: errText}
	}

	d.volumes[name].MountCount--
	if d.volumes[name].MountCount > 0 {
		logger.Info("volume-still-in-use", lager.Data{"name": name, "count": d.volumes[name].MountCount})
		return voldriver.ErrorResponse{}
	} else {
		logger.Info("unmount-volume-folder", lager.Data{"mountpath": mountPath})
		args := []string{mountPath}
		err := d.invoker.Invoke(logger, "rm", args)
		if err != nil {
			logger.Error("unmount-failed", err)
			return voldriver.ErrorResponse{Err: fmt.Sprintf("Error mounting volume: %s", err.Error())}
		}
	}

	logger.Info("unmounted-volume")

	d.volumes[name].Mountpoint = ""

	return voldriver.ErrorResponse{}
}
