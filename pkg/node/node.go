package node

import (
	"context"
	"encoding/json"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"sigs.k8s.io/container-object-storage-interface-csi-adapter/pkg/client"
	"sigs.k8s.io/container-object-storage-interface-csi-adapter/pkg/util"
)

var _ csi.NodeServer = &NodeServer{}

const (
	credsFileName    = "credentials"
	protocolFileName = "protocolConn.json"
	metadataFilename = "metadata.json"
)

func NewNodeServerOrDie(driverName, nodeID, dataRoot string, volumeLimit int64) csi.NodeServer {
	cosiClient := client.NewClientOrDie(driverName, nodeID)
	return &NodeServer{
		name:        driverName,
		nodeID:      nodeID,
		volumeLimit: volumeLimit,
		cosiClient:  cosiClient,
		provisioner: NewProvisioner(dataRoot, mount.New(""), client.NewProvisionerClient()),
	}
}

// NodeServer implements the NodePublishVolume and NodeUnpublishVolume methods
// of the csi.NodeServer
type NodeServer struct {
	csi.UnimplementedNodeServer
	name        string
	nodeID      string
	volumeLimit int64
	cosiClient  client.NodeClient
	provisioner Provisioner
}

func (n *NodeServer) NodePublishVolume(ctx context.Context, request *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume: volId: %v, targetPath: %v\n", request.GetVolumeId(), request.GetTargetPath())

	barName, podName, podNs, err := client.ParseVolumeContext(request.GetVolumeContext())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	bkt, ba, secret, pod, err := n.cosiClient.GetResources(ctx, barName, podName, podNs)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	protocolConnection, err := client.GetProtocol(bkt)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	klog.Infof("bucket %q has protocol %q", bkt.Name, bkt.Spec.Protocol)

	if err := n.provisioner.createDir(request.GetVolumeId()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	cleanup := func(err error, errWrap string) (*csi.NodePublishVolumeResponse, error) {
		rmErr := errors.Wrap(n.provisioner.removeDir(request.GetVolumeId()), util.WrapErrorFailedRemoveDirectory)
		if rmErr != nil {
			return nil, status.Error(codes.Internal, errors.Wrap(rmErr, errWrap).Error())
		}
		return nil, status.Error(codes.Internal, errors.Wrap(err, errWrap).Error())
	}

	creds, err := util.ParseData(secret)
	if err != nil {
		return cleanup(err, util.WrapErrorFailedToParseSecret)
	}

	if err := n.provisioner.writeFileToVolumeMount(protocolConnection, request.GetVolumeId(), protocolFileName); err != nil {
		return cleanup(err, util.WrapErrorFailedToWriteProtocol)
	}

	if err := n.provisioner.writeFileToVolumeMount(creds, request.GetVolumeId(), credsFileName); err != nil {
		return cleanup(err, util.WrapErrorFailedToWriteCredentials)
	}

	util.EmitNormalEvent(n.cosiClient.Recorder(), pod, util.CredentialsWritten)

	err = n.provisioner.mountDir(request.GetVolumeId(), request.GetTargetPath())
	if err != nil {
		return cleanup(err, util.WrapErrorFailedToMountVolume)
	}

	meta := Metadata{
		BaName:       ba.Name,
		PodName:      podName,
		PodNamespace: podNs,
	}

	err = n.cosiClient.AddBAFinalizer(ctx, ba, meta.finalizer())
	if err != nil {
		return cleanup(err, util.WrapErrorFailedToAddFinalizer)
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return cleanup(err, util.WrapErrorFailedToMarshalMetadata)
	}

	// Write the BA.name to a metadata file in our volume, this is not mounted to the app pod
	if err := n.provisioner.writeFileToVolume(data, request.GetVolumeId(), metadataFilename); err != nil {
		return cleanup(err, util.WrapErrorFailedToWriteMetadata)
	}

	util.EmitNormalEvent(n.cosiClient.Recorder(), pod, util.SuccessfullyPublishedVolume)

	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *NodeServer) NodeUnpublishVolume(ctx context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.Infof("NodeUnpublishVolume: volId: %v, targetPath: %v\n", request.GetVolumeId(), request.GetTargetPath())

	data, err := n.provisioner.readFileFromVolume(request.GetVolumeId(), metadataFilename)
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, util.WrapErrorFailedToReadMetadataFile).Error())
	}

	meta := Metadata{}
	err = json.Unmarshal(data, &meta)
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, util.WrapErrorFailedToUnmarshalMetadata).Error())
	}

	klog.InfoS("read metadata file", "metadata", meta)

	pod, err := n.cosiClient.GetPod(ctx, meta.PodName, meta.PodNamespace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	ba, err := n.cosiClient.GetBA(ctx, pod, meta.BaName)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = n.provisioner.removeMount(request.GetTargetPath())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = n.provisioner.removeDir(request.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, util.WrapErrorFailedToRemoveDir).Error())
	}

	err = n.cosiClient.RemoveBAFinalizer(ctx, ba, meta.finalizer())
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, util.WrapErrorFailedToRemoveFinalizer).Error())
	}

	util.EmitNormalEvent(n.cosiClient.Recorder(), pod, util.SuccessfullyUnpublishedVolume)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *NodeServer) NodeGetInfo(ctx context.Context, request *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{
		NodeId:            n.nodeID,
		MaxVolumesPerNode: n.volumeLimit,
	}
	return resp, nil
}

func (n *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
