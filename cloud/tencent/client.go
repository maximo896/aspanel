package tencent

import (
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

var instanceTypeSpecs = map[string][2]int{
	"S5.SMALL1":    {1, 2},
	"S5.SMALL2":    {2, 2},
	"S5.MEDIUM2":   {2, 4},
	"SA2.SMALL1":   {1, 2},
	"SA2.MEDIUM2":  {2, 4},
	"S5.LARGE8":    {4, 8},
	"SA2.LARGE8":   {4, 8},
	"S5.XLARGE16":  {8, 16},
	"SA2.XLARGE16": {8, 16},
}

func InstanceTypeSpec(instanceType string) (int, int, bool) {
	spec, ok := instanceTypeSpecs[strings.TrimSpace(strings.ToUpper(instanceType))]
	if !ok {
		return 0, 0, false
	}
	return spec[0], spec[1], true
}

func RecommendInstanceTypes(minCPU, minMemoryGB int) []string {
	if minCPU <= 0 {
		minCPU = 1
	}
	if minMemoryGB <= 0 {
		minMemoryGB = 1
	}
	types := make([]string, 0, len(instanceTypeSpecs))
	for t, spec := range instanceTypeSpecs {
		if spec[0] >= minCPU && spec[1] >= minMemoryGB {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	return types
}

type Client struct {
	settings Settings
}

func NewClient(settings Settings) *Client {
	settings.SecretID = strings.TrimSpace(settings.SecretID)
	settings.SecretKey = strings.TrimSpace(settings.SecretKey)
	settings.SecretID = strings.ReplaceAll(settings.SecretID, "\r", "")
	settings.SecretID = strings.ReplaceAll(settings.SecretID, "\n", "")
	settings.SecretKey = strings.ReplaceAll(settings.SecretKey, "\r", "")
	settings.SecretKey = strings.ReplaceAll(settings.SecretKey, "\n", "")
	return &Client{settings: settings}
}

func (c *Client) cvmClient(region string) (*cvm.Client, error) {
	cred := common.NewCredential(c.settings.SecretID, c.settings.SecretKey)
	pf := profile.NewClientProfile()
	return cvm.NewClient(cred, region, pf)
}

func (c *Client) vpcClient(region string) (*vpc.Client, error) {
	cred := common.NewCredential(c.settings.SecretID, c.settings.SecretKey)
	pf := profile.NewClientProfile()
	return vpc.NewClient(cred, region, pf)
}

func (c *Client) ListZones(region string) ([]string, error) {
	client, err := c.cvmClient(region)
	if err != nil {
		return nil, err
	}
	req := cvm.NewDescribeZonesRequest()
	resp, err := client.DescribeZones(req)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, z := range resp.Response.ZoneSet {
		if z.Zone != nil {
			out = append(out, *z.Zone)
		}
	}
	return out, nil
}

func (c *Client) ListSpotOffers(region, instanceType string) ([]SpotOffer, error) {
	client, err := c.cvmClient(region)
	if err != nil {
		return nil, err
	}
	req := cvm.NewDescribeZoneInstanceConfigInfosRequest()
	filters := []*cvm.Filter{
		{
			Name:   common.StringPtr("instance-charge-type"),
			Values: common.StringPtrs([]string{"SPOTPAID"}),
		},
	}
	if strings.TrimSpace(instanceType) != "" {
		filters = append(filters, &cvm.Filter{
			Name:   common.StringPtr("instance-type"),
			Values: common.StringPtrs([]string{strings.TrimSpace(instanceType)}),
		})
	}
	req.Filters = filters
	resp, err := client.DescribeZoneInstanceConfigInfos(req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Response == nil {
		return nil, fmt.Errorf("empty zone instance config response for %s in %s", instanceType, region)
	}

	offers := make([]SpotOffer, 0, len(resp.Response.InstanceTypeQuotaSet))
	for _, item := range resp.Response.InstanceTypeQuotaSet {
		if item == nil || item.Zone == nil || item.InstanceType == nil || item.Cpu == nil || item.Memory == nil {
			continue
		}
		if item.Status != nil && strings.ToUpper(strings.TrimSpace(*item.Status)) != "SELL" {
			continue
		}
		zone := strings.TrimSpace(*item.Zone)
		it := strings.TrimSpace(*item.InstanceType)
		cpu := int(*item.Cpu)
		mem := int(*item.Memory)
		if zone == "" || it == "" || cpu <= 0 || mem <= 0 {
			continue
		}
		priceCNY := 0.0
		if item.Price != nil {
			if item.Price.UnitPriceDiscount != nil && *item.Price.UnitPriceDiscount > 0 {
				priceCNY = *item.Price.UnitPriceDiscount
			} else if item.Price.UnitPrice != nil && *item.Price.UnitPrice > 0 {
				priceCNY = *item.Price.UnitPrice
			}
		}
		if priceCNY <= 0 {
			continue
		}
		offers = append(offers, SpotOffer{
			Region:           region,
			Zone:             zone,
			InstanceType:     it,
			PriceUSD:         priceCNY,
			InstancePriceUSD: priceCNY,
			ExtraPriceUSD:    0,
			ConfigPriceUSD:   priceCNY,
			CPU:              cpu,
			MemoryGB:         mem,
		})
	}
	return offers, nil
}

func pickItemUnitPrice(price *cvm.ItemPrice) float64 {
	if price == nil {
		return 0
	}
	if price.UnitPriceDiscount != nil && *price.UnitPriceDiscount > 0 {
		return *price.UnitPriceDiscount
	}
	if price.UnitPrice != nil && *price.UnitPrice > 0 {
		return *price.UnitPrice
	}
	return 0
}

func (c *Client) InquirySpotConfiguredPrice(req SpotPriceInquiryRequest) (instancePriceUSD, bandwidthPriceUSD, totalPriceUSD float64, err error) {
	client, err := c.cvmClient(req.Region)
	if err != nil {
		return 0, 0, 0, err
	}
	inquiryReq := cvm.NewInquiryPriceRunInstancesRequest()
	inquiryReq.Placement = &cvm.Placement{
		Zone: common.StringPtr(req.Zone),
	}
	inquiryReq.ImageId = common.StringPtr(req.ImageID)
	inquiryReq.InstanceChargeType = common.StringPtr("SPOTPAID")
	inquiryReq.InstanceType = common.StringPtr(req.InstanceType)
	inquiryReq.InstanceCount = common.Int64Ptr(1)
	inquiryReq.InternetAccessible = &cvm.InternetAccessible{
		PublicIpAssigned:        common.BoolPtr(true),
		InternetChargeType:      common.StringPtr("TRAFFIC_POSTPAID_BY_HOUR"),
		InternetMaxBandwidthOut: common.Int64Ptr(100),
	}
	inquiryReq.InstanceMarketOptions = &cvm.InstanceMarketOptionsRequest{
		MarketType: common.StringPtr("spot"),
		SpotOptions: &cvm.SpotMarketOptions{
			MaxPrice:         common.StringPtr(fmt.Sprintf("%.4f", req.MaxPriceUSD)),
			SpotInstanceType: common.StringPtr("one-time"),
		},
	}
	resp, err := client.InquiryPriceRunInstances(inquiryReq)
	if err != nil {
		return 0, 0, 0, err
	}
	if resp == nil || resp.Response == nil || resp.Response.Price == nil {
		return 0, 0, 0, fmt.Errorf("empty inquiry price response")
	}
	instancePriceUSD = pickItemUnitPrice(resp.Response.Price.InstancePrice)
	bandwidthPriceUSD = pickItemUnitPrice(resp.Response.Price.BandwidthPrice)
	totalPriceUSD = instancePriceUSD + bandwidthPriceUSD
	if totalPriceUSD <= 0 {
		totalPriceUSD = instancePriceUSD
	}
	return instancePriceUSD, bandwidthPriceUSD, totalPriceUSD, nil
}

func (c *Client) RunSpotInstances(req LaunchRequest) ([]string, error) {
	client, err := c.cvmClient(req.Region)
	if err != nil {
		return nil, err
	}
	runReq := cvm.NewRunInstancesRequest()
	runReq.InstanceChargeType = common.StringPtr("SPOTPAID")
	runReq.InstanceCount = common.Int64Ptr(req.Count)
	runReq.InstanceType = common.StringPtr(req.InstanceType)
	runReq.ImageId = common.StringPtr(req.ImageID)
	runReq.Placement = &cvm.Placement{
		Zone: common.StringPtr(req.Zone),
	}
	runReq.InternetAccessible = &cvm.InternetAccessible{
		PublicIpAssigned:        common.BoolPtr(true),
		InternetChargeType:      common.StringPtr("TRAFFIC_POSTPAID_BY_HOUR"),
		InternetMaxBandwidthOut: common.Int64Ptr(100),
	}
	runReq.InstanceMarketOptions = &cvm.InstanceMarketOptionsRequest{
		MarketType: common.StringPtr("spot"),
		SpotOptions: &cvm.SpotMarketOptions{
			MaxPrice:         common.StringPtr(fmt.Sprintf("%.4f", req.MaxPriceUSD)),
			SpotInstanceType: common.StringPtr("one-time"),
		},
	}
	if req.KeyID != "" {
		runReq.LoginSettings = &cvm.LoginSettings{
			KeyIds: common.StringPtrs([]string{req.KeyID}),
		}
	} else if req.Password != "" {
		runReq.LoginSettings = &cvm.LoginSettings{
			Password: common.StringPtr(req.Password),
		}
	}
	if len(req.SecurityIDs) > 0 {
		runReq.SecurityGroupIds = common.StringPtrs(req.SecurityIDs)
	}
	if req.VpcID != "" && req.SubnetID != "" {
		runReq.VirtualPrivateCloud = &cvm.VirtualPrivateCloud{
			VpcId:    common.StringPtr(req.VpcID),
			SubnetId: common.StringPtr(req.SubnetID),
		}
	}
	if req.UserDataB64 != "" {
		runReq.UserData = common.StringPtr(req.UserDataB64)
	}
	resp, err := client.RunInstances(runReq)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Response.InstanceIdSet))
	for _, id := range resp.Response.InstanceIdSet {
		if id != nil {
			out = append(out, *id)
		}
	}
	return out, nil
}

func (c *Client) ResolveUbuntuImageID(region string) (string, error) {
	client, err := c.cvmClient(region)
	if err != nil {
		return "", err
	}
	req := cvm.NewDescribeImagesRequest()
	req.Filters = []*cvm.Filter{
		{
			Name:   common.StringPtr("image-type"),
			Values: common.StringPtrs([]string{"PUBLIC_IMAGE"}),
		},
		{
			Name:   common.StringPtr("image-name"),
			Values: common.StringPtrs([]string{"Ubuntu Server 24.04"}),
		},
	}
	resp, err := client.DescribeImages(req)
	if err != nil {
		return "", err
	}
	type imageItem struct {
		id   string
		name string
	}
	items := make([]imageItem, 0, len(resp.Response.ImageSet))
	for _, img := range resp.Response.ImageSet {
		if img.ImageId == nil || img.ImageName == nil {
			continue
		}
		items = append(items, imageItem{id: *img.ImageId, name: *img.ImageName})
	}
	if len(items) == 0 {
		return "", fmt.Errorf("no ubuntu 24.04 public image found in %s", region)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].name < items[j].name
	})
	return items[0].id, nil
}

func (c *Client) ResolveDefaultVpcSubnet(region, zone string) (string, string, error) {
	client, err := c.vpcClient(region)
	if err != nil {
		return "", "", err
	}

	vpcReq := vpc.NewDescribeVpcsRequest()
	vpcResp, err := client.DescribeVpcs(vpcReq)
	if err != nil {
		return "", "", err
	}
	if len(vpcResp.Response.VpcSet) == 0 {
		vpcID, vpcCIDR, err := c.createAutoVPC(region)
		if err != nil {
			return "", "", fmt.Errorf("no vpc found in %s and auto-create failed: %w", region, err)
		}
		subnetID, err := c.createSubnetForZone(region, vpcID, vpcCIDR, zone)
		if err != nil {
			return "", "", fmt.Errorf("auto-created vpc %s but failed to create subnet for zone %s: %w", vpcID, zone, err)
		}
		return vpcID, subnetID, nil
	}
	vpcID := ""
	vpcCIDR := ""
	for _, v := range vpcResp.Response.VpcSet {
		if v.VpcId != nil {
			vpcID = *v.VpcId
			if v.CidrBlock != nil {
				vpcCIDR = strings.TrimSpace(*v.CidrBlock)
			}
			if v.IsDefault != nil && *v.IsDefault {
				break
			}
		}
	}
	if vpcID == "" {
		return "", "", fmt.Errorf("no valid vpc id found in %s", region)
	}

	subnetReq := vpc.NewDescribeSubnetsRequest()
	subnetReq.Filters = []*vpc.Filter{
		{
			Name:   common.StringPtr("vpc-id"),
			Values: common.StringPtrs([]string{vpcID}),
		},
	}
	subnetResp, err := client.DescribeSubnets(subnetReq)
	if err != nil {
		return "", "", err
	}
	if len(subnetResp.Response.SubnetSet) == 0 {
		return "", "", fmt.Errorf("no subnet found in vpc %s", vpcID)
	}
	subnetID := ""
	zone = strings.TrimSpace(zone)
	// Prefer a subnet in the same zone as the instance to avoid SubnetIdZoneIdNotMatch.
	if zone != "" {
		for _, s := range subnetResp.Response.SubnetSet {
			if s.SubnetId == nil || s.Zone == nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(*s.Zone), zone) {
				subnetID = *s.SubnetId
				break
			}
		}
	}
	for _, s := range subnetResp.Response.SubnetSet {
		if s.SubnetId != nil {
			if zone != "" && s.Zone != nil && !strings.EqualFold(strings.TrimSpace(*s.Zone), zone) {
				continue
			}
			subnetID = *s.SubnetId
			if s.IsDefault != nil && *s.IsDefault {
				break
			}
		}
	}
	if subnetID == "" {
		createdSubnetID, err := c.createSubnetForZone(region, vpcID, vpcCIDR, zone)
		if err != nil {
			if zone != "" {
				return "", "", fmt.Errorf("no valid subnet id found in vpc %s for zone %s and auto-create failed: %w", vpcID, zone, err)
			}
			return "", "", fmt.Errorf("no valid subnet id found in vpc %s and auto-create failed: %w", vpcID, err)
		}
		subnetID = createdSubnetID
	}
	return vpcID, subnetID, nil
}

func (c *Client) createAutoVPC(region string) (string, string, error) {
	client, err := c.vpcClient(region)
	if err != nil {
		return "", "", err
	}
	req := vpc.NewCreateVpcRequest()
	req.VpcName = common.StringPtr("awvs-sqlmap-auto-vpc")
	req.CidrBlock = common.StringPtr("10.66.0.0/16")
	resp, err := client.CreateVpc(req)
	if err != nil {
		return "", "", err
	}
	if resp.Response == nil || resp.Response.Vpc == nil || resp.Response.Vpc.VpcId == nil {
		return "", "", fmt.Errorf("create vpc returned empty vpc id")
	}
	cidr := "10.66.0.0/16"
	if resp.Response.Vpc.CidrBlock != nil && strings.TrimSpace(*resp.Response.Vpc.CidrBlock) != "" {
		cidr = strings.TrimSpace(*resp.Response.Vpc.CidrBlock)
	}
	return strings.TrimSpace(*resp.Response.Vpc.VpcId), cidr, nil
}

func (c *Client) createSubnetForZone(region, vpcID, vpcCIDR, zone string) (string, error) {
	client, err := c.vpcClient(region)
	if err != nil {
		return "", err
	}
	zone = strings.TrimSpace(zone)
	existingReq := vpc.NewDescribeSubnetsRequest()
	existingReq.Filters = []*vpc.Filter{
		{
			Name:   common.StringPtr("vpc-id"),
			Values: common.StringPtrs([]string{vpcID}),
		},
	}
	existingResp, err := client.DescribeSubnets(existingReq)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, s := range existingResp.Response.SubnetSet {
		if s.CidrBlock != nil {
			used[strings.TrimSpace(*s.CidrBlock)] = true
		}
	}

	candidates := subnetCIDRCandidates(vpcCIDR, used)
	if len(candidates) == 0 {
		candidates = []string{"10.66.10.0/24", "10.66.11.0/24", "10.66.12.0/24", "10.66.13.0/24"}
	}
	var lastErr error
	for idx, cidr := range candidates {
		req := vpc.NewCreateSubnetRequest()
		req.VpcId = common.StringPtr(vpcID)
		req.Zone = common.StringPtr(zone)
		req.SubnetName = common.StringPtr(fmt.Sprintf("awvs-sqlmap-auto-subnet-%d", idx))
		req.CidrBlock = common.StringPtr(cidr)
		resp, err := client.CreateSubnet(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Response != nil && resp.Response.Subnet != nil && resp.Response.Subnet.SubnetId != nil {
			return strings.TrimSpace(*resp.Response.Subnet.SubnetId), nil
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("unable to create subnet in zone %s for vpc %s: %w", zone, vpcID, lastErr)
	}
	return "", fmt.Errorf("unable to create subnet in zone %s for vpc %s", zone, vpcID)
}

func subnetCIDRCandidates(vpcCIDR string, used map[string]bool) []string {
	_, network, err := net.ParseCIDR(strings.TrimSpace(vpcCIDR))
	if err != nil {
		return nil
	}
	base := network.IP.To4()
	if base == nil {
		return nil
	}
	ones, bits := network.Mask.Size()
	if bits != 32 {
		return nil
	}
	if ones >= 24 {
		cidr := strings.TrimSpace(vpcCIDR)
		if !used[cidr] {
			return []string{cidr}
		}
		return nil
	}
	maxBlocks := 1 << (24 - ones)
	if maxBlocks > 256 {
		maxBlocks = 256
	}
	out := make([]string, 0, maxBlocks)
	for i := 0; i < maxBlocks; i++ {
		ip := make(net.IP, 4)
		copy(ip, base)
		ip[2] = byte(i)
		ip[3] = 0
		cidr := fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], ip[2])
		if used[cidr] {
			continue
		}
		out = append(out, cidr)
	}
	return out
}

func (c *Client) EnsureAllowAllSecurityGroup(region, vpcID string) (string, error) {
	client, err := c.vpcClient(region)
	if err != nil {
		return "", err
	}
	sgName := "awvs-sqlmap-open-all"

	sgReq := vpc.NewDescribeSecurityGroupsRequest()
	sgReq.Filters = []*vpc.Filter{
		{
			Name:   common.StringPtr("security-group-name"),
			Values: common.StringPtrs([]string{sgName}),
		},
	}
	sgResp, err := client.DescribeSecurityGroups(sgReq)
	if err == nil {
		for _, sg := range sgResp.Response.SecurityGroupSet {
			if sg.SecurityGroupId != nil {
				return *sg.SecurityGroupId, nil
			}
		}
	}

	createReq := vpc.NewCreateSecurityGroupRequest()
	createReq.GroupName = common.StringPtr(sgName)
	createReq.GroupDescription = common.StringPtr("auto created by awvs-sqlmap panel")
	createResp, err := client.CreateSecurityGroup(createReq)
	if err != nil {
		return "", err
	}
	if createResp.Response.SecurityGroup == nil || createResp.Response.SecurityGroup.SecurityGroupId == nil {
		return "", fmt.Errorf("create security group failed")
	}
	sgID := *createResp.Response.SecurityGroup.SecurityGroupId

	ingressReq := vpc.NewCreateSecurityGroupPoliciesRequest()
	ingressReq.SecurityGroupId = common.StringPtr(sgID)
	ingressReq.SecurityGroupPolicySet = &vpc.SecurityGroupPolicySet{
		Ingress: []*vpc.SecurityGroupPolicy{
			{
				PolicyIndex:       common.Int64Ptr(0),
				Protocol:          common.StringPtr("ALL"),
				Port:              common.StringPtr("ALL"),
				CidrBlock:         common.StringPtr("0.0.0.0/0"),
				Action:            common.StringPtr("ACCEPT"),
				PolicyDescription: common.StringPtr("allow all ingress"),
			},
		},
	}
	_, err = client.CreateSecurityGroupPolicies(ingressReq)
	if err != nil {
		return "", err
	}

	egressReq := vpc.NewCreateSecurityGroupPoliciesRequest()
	egressReq.SecurityGroupId = common.StringPtr(sgID)
	egressReq.SecurityGroupPolicySet = &vpc.SecurityGroupPolicySet{
		Egress: []*vpc.SecurityGroupPolicy{
			{
				PolicyIndex:       common.Int64Ptr(0),
				Protocol:          common.StringPtr("ALL"),
				Port:              common.StringPtr("ALL"),
				CidrBlock:         common.StringPtr("0.0.0.0/0"),
				Action:            common.StringPtr("ACCEPT"),
				PolicyDescription: common.StringPtr("allow all egress"),
			},
		},
	}
	_, err = client.CreateSecurityGroupPolicies(egressReq)
	if err != nil {
		return "", err
	}
	return sgID, nil
}

func EncodeUserData(script string) string {
	return base64.StdEncoding.EncodeToString([]byte(script))
}

func (c *Client) DescribeInstances(region string, ids []string) ([]Instance, error) {
	client, err := c.cvmClient(region)
	if err != nil {
		return nil, err
	}
	req := cvm.NewDescribeInstancesRequest()
	if len(ids) > 0 {
		req.InstanceIds = common.StringPtrs(ids)
	}
	resp, err := client.DescribeInstances(req)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(resp.Response.InstanceSet))
	for _, ins := range resp.Response.InstanceSet {
		item := Instance{Region: region}
		if ins.InstanceId != nil {
			item.InstanceID = *ins.InstanceId
		}
		if ins.Placement != nil && ins.Placement.Zone != nil {
			item.Zone = *ins.Placement.Zone
		}
		if ins.InstanceState != nil {
			item.Status = strings.ToLower(*ins.InstanceState)
		}
		if ins.InstanceType != nil {
			item.InstanceType = strings.TrimSpace(*ins.InstanceType)
		}
		if ins.CPU != nil && *ins.CPU > 0 {
			item.CPU = int(*ins.CPU)
		}
		if ins.Memory != nil && *ins.Memory > 0 {
			item.MemoryGB = int(*ins.Memory)
		}
		out = append(out, item)
	}
	return out, nil
}

func (c *Client) TerminateInstances(region string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	client, err := c.cvmClient(region)
	if err != nil {
		return err
	}
	req := cvm.NewTerminateInstancesRequest()
	req.InstanceIds = common.StringPtrs(ids)
	_, err = client.TerminateInstances(req)
	return err
}

func (c *Client) RebootInstances(region string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	client, err := c.cvmClient(region)
	if err != nil {
		return err
	}
	req := cvm.NewRebootInstancesRequest()
	req.InstanceIds = common.StringPtrs(ids)
	req.StopType = common.StringPtr("SOFT_FIRST")
	_, err = client.RebootInstances(req)
	return err
}
