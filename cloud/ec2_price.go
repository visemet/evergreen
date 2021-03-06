package cloud

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
)

type timeRange struct {
	start time.Time
	end   time.Time
}

type cachingPriceFetcher struct {
	ec2Prices map[odInfo]float64
	ebsPrices map[string]float64
	sync.Mutex
}

// spotRate is an internal type for simplifying Amazon's price history responses.
type spotRate struct {
	Time  time.Time
	Price float64
}

var pkgCachingPriceFetcher *cachingPriceFetcher

func init() {
	pkgCachingPriceFetcher = new(cachingPriceFetcher)
}

func (cpf *cachingPriceFetcher) getEC2Cost(ctx context.Context, client AWSClient, h *host.Host, t timeRange) (float64, error) {
	os := getOsName(h)
	if isHostOnDemand(h) {
		instance, err := client.GetInstanceInfo(ctx, h.Id)
		if err != nil {
			return 0, errors.Wrap(err, "error getting instance info")
		}
		dur := t.end.Sub(t.start)
		region := azToRegion(*instance.Placement.AvailabilityZone)
		price, err := cpf.getEC2OnDemandCost(os, *instance.InstanceType, region)
		if err != nil {
			return 0, err
		}
		return price * dur.Hours(), nil
	}
	instanceId, err := client.GetSpotInstanceId(ctx, h)
	if err != nil {
		return 0, errors.Wrap(err, "error getting spot instance ID")
	}
	if instanceId == "" {
		return 0, errors.WithStack(errors.New("spot instance does not yet have an instanceId"))
	}
	instance, err := client.GetInstanceInfo(ctx, instanceId)
	if err != nil {
		return 0, errors.Wrap(err, "error getting instance info")
	}
	return cpf.calculateSpotCost(ctx, client, instance, os, t)
}

func (cpf *cachingPriceFetcher) getEC2OnDemandCost(os osType, instance, region string) (float64, error) {
	cpf.Lock()
	defer cpf.Unlock()
	if cpf.ec2Prices == nil {
		if err := cpf.cacheEc2Prices(); err != nil {
			return 0, errors.Wrap(err, "loading On Demand price data")
		}
	}
	region, err := regionFullname(region)
	if err != nil {
		return 0, err
	}
	return cpf.ec2Prices[odInfo{
		os: osBillingName(os), instance: instance, region: region,
	}], nil
}

func (cpf *cachingPriceFetcher) cacheEc2Prices() error {
	cpf.ec2Prices = map[odInfo]float64{}
	// the On Demand pricing API is not part of the normal EC2 API
	endpoint := "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/index.json"
	grip.Debugln("Loading On Demand pricing from", endpoint)

	client := util.GetHTTPClient()
	defer util.PutHTTPClient(client)

	details := struct {
		Terms    Terms
		Products map[string]struct {
			SKU           string
			ProductFamily string
			Attributes    struct {
				Location        string
				InstanceType    string
				PreInstalledSW  string
				OperatingSystem string
				Tenancy         string
				LicenseModel    string
			}
		}
	}{}

	_, err := util.Retry(func() (bool, error) {
		resp, err := client.Get(endpoint)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			return true, errors.Wrapf(err, "fetching %v", endpoint)
		}
		grip.Debug("Parsing on-demand pricing")

		if err = json.NewDecoder(resp.Body).Decode(&details); err != nil {
			return true, errors.Wrap(err, "parsing response body")
		}
		return false, nil
	}, awsClientImplRetries, awsClientImplStartPeriod)

	if err != nil {
		return errors.WithStack(err)
	}

	for _, p := range details.Products {
		if p.ProductFamily == "Compute Instance" &&
			p.Attributes.PreInstalledSW == "NA" &&
			p.Attributes.Tenancy == "Shared" &&
			p.Attributes.LicenseModel != "Bring your own license" {
			// the product description does not include pricing information,
			// so we must look up the SKU in the "Terms" section.
			price := details.Terms.skuPrice(p.SKU)
			cpf.ec2Prices[odInfo{
				os:       p.Attributes.OperatingSystem,
				instance: p.Attributes.InstanceType,
				region:   p.Attributes.Location,
			}] = price
		}
	}
	grip.Debug("Finished parsing on-demand pricing")
	return nil
}

func (cpf *cachingPriceFetcher) getLatestLowestSpotCostForInstance(ctx context.Context, client AWSClient, settings *EC2ProviderSettings, os osType) (float64, string, error) {
	cpf.Lock()
	defer cpf.Unlock()
	osName := string(os)
	if settings.IsVpc {
		osName += " (Amazon VPC)"
	}
	grip.Debug(message.Fields{
		"message":       "getting spot history",
		"instance_type": settings.InstanceType,
		"os_name":       osName,
	})
	prices, err := client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		// passing a future start time gets the latest price only
		StartTime:           makeTimePtr(time.Now().UTC().Add(24 * time.Hour)),
		InstanceTypes:       []*string{makeStringPtr(settings.InstanceType)},
		ProductDescriptions: []*string{makeStringPtr(osName)},
	})
	if err != nil {
		return 0, "", errors.Wrap(err, "error getting spot price history")
	}
	if len(prices.SpotPriceHistory) == 0 {
		return 0, "", errors.New("no prices found")
	}
	var min float64
	var az string
	for i := range prices.SpotPriceHistory {
		p, err := strconv.ParseFloat(*prices.SpotPriceHistory[i].SpotPrice, 0)
		if err != nil {
			return 0, "", errors.Wrap(err, "error parsing spot price")
		}
		if min == 0 || p < min {
			min = p
			az = *prices.SpotPriceHistory[i].AvailabilityZone
		}
	}
	return min, az, nil
}

func (m *ec2Manager) getProvider(ctx context.Context, h *host.Host, ec2settings *EC2ProviderSettings) (ec2ProviderType, error) {
	if h.UserHost {
		h.Distro.Provider = evergreen.ProviderNameEc2OnDemand
		return onDemandProvider, nil
	}
	if m.provider == spotProvider {
		h.Distro.Provider = evergreen.ProviderNameEc2Spot
		return spotProvider, nil
	}
	if m.provider == onDemandProvider {
		h.Distro.Provider = evergreen.ProviderNameEc2OnDemand
		return onDemandProvider, nil
	}
	if m.provider == autoProvider {
		onDemandPrice, err := pkgCachingPriceFetcher.getEC2OnDemandCost(getOsName(h), ec2settings.InstanceType, defaultRegion)
		if err != nil {
			return 0, errors.Wrap(err, "error getting ec2 on-demand cost")
		}

		spotPrice, az, err := pkgCachingPriceFetcher.getLatestLowestSpotCostForInstance(ctx, m.client, ec2settings, getOsName(h))
		if err != nil {
			return 0, errors.Wrap(err, "error getting latest lowest spot price")
		}
		if spotPrice < onDemandPrice {
			ec2settings.BidPrice = onDemandPrice
			if ec2settings.VpcName != "" {
				subnetID, err := m.getSubnetForAZ(ctx, az, ec2settings.VpcName)
				if err != nil {
					return 0, errors.Wrap(err, "error settings dynamic subnet for spot")
				}
				ec2settings.SubnetId = subnetID
			}
			h.Distro.Provider = evergreen.ProviderNameEc2Spot
			return spotProvider, nil
		}
		h.Distro.Provider = evergreen.ProviderNameEc2OnDemand
		return onDemandProvider, nil
	}
	return 0, errors.Errorf("provider is %d, expected %d, %d, or %d", m.provider, onDemandProvider, spotProvider, autoProvider)
}

func (m *ec2Manager) getSubnetForAZ(ctx context.Context, azName, vpcName string) (string, error) {
	vpcs, err := m.client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name: makeStringPtr("tag:Name"),
				Values: []*string{
					makeStringPtr(vpcName),
				},
			},
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "error finding vpc id")
	}
	vpcID := *vpcs.Vpcs[0].VpcId

	subnets, err := m.client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   makeStringPtr("vpc-id"),
				Values: []*string{makeStringPtr(vpcID)},
			},
			&ec2.Filter{
				Name:   makeStringPtr("availability-zone"),
				Values: []*string{makeStringPtr(azName)},
			},
			&ec2.Filter{
				Name:   makeStringPtr("tag:Name"),
				Values: []*string{makeStringPtr(vpcName + ".subnet_" + strings.Split(azName, "-")[2])},
			},
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "error finding subnet id")
	}
	return *subnets.Subnets[0].SubnetId, nil
}

func (cpf *cachingPriceFetcher) getEBSCost(ctx context.Context, client AWSClient, h *host.Host, t timeRange) (float64, error) {
	cpf.Lock()
	defer cpf.Unlock()
	instanceID := h.Id
	var err error
	if isHostSpot(h) {
		instanceID, err = client.GetSpotInstanceId(ctx, h)
		if err != nil {
			return 0, errors.Wrap(err, "error getting spot instance ID")
		}
		if instanceID == "" {
			return 0, errors.WithStack(errors.New("spot instance does not yet have an instanceId"))
		}
	}
	instance, err := client.GetInstanceInfo(ctx, instanceID)
	if err != nil {
		return 0, errors.Wrap(err, "error getting instance info")
	}
	dur := t.end.Sub(t.start)
	devices := instance.BlockDeviceMappings
	cost := 0.0
	if len(devices) > 0 {
		volumeIds := []*string{}
		for i := range devices {
			volumeIds = append(volumeIds, devices[i].Ebs.VolumeId)
		}
		vols, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: volumeIds,
		})
		if err != nil {
			return 0, err
		}
		for _, v := range vols.Volumes {
			// an amazon region is just the availability zone minus the final letter
			region := azToRegion(*v.AvailabilityZone)
			p, err := cpf.ebsCost(region, *v.Size, dur)
			if err != nil {
				return 0, errors.Wrapf(err, "EBS volume %v", v.VolumeId)
			}
			cost += p
		}
	}
	return cost, nil
}

// ebsCost returns the cost of running an EBS block device for an amount of time in a given size and region.
// EBS bills are charged in "GB/Month" units. We consider a month to be 30 days.
func (cpf *cachingPriceFetcher) ebsCost(region string, size int64, duration time.Duration) (float64, error) {
	if cpf.ebsPrices == nil {
		if err := cpf.cacheEBSPrices(); err != nil {
			return 0, errors.Wrap(err, "error fetching EBS prices")
		}
	}
	price, ok := cpf.ebsPrices[region]
	if !ok {
		return 0.0, errors.Errorf("no EBS price for region '%v'", region)
	}
	// price = GB * % of month *
	month := (time.Hour * 24 * 30)
	return float64(size) * (float64(duration) / float64(month)) * price, nil

}

// fetchEBSPricing does the dirty work of scraping price information from Amazon.
func (cpf *cachingPriceFetcher) cacheEBSPrices() error {
	// there is no true EBS pricing API, so we have to wrangle it from EC2's frontend
	endpoint := "http://a0.awsstatic.com/pricing/1/ebs/pricing-ebs.js"
	grip.Debugln("Loading EBS pricing from", endpoint)

	client := util.GetHTTPClient()
	defer util.PutHTTPClient(client)

	var data []byte

	_, err := util.Retry(func() (bool, error) {
		resp, err := client.Get(endpoint)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			return true, errors.Wrapf(err, "fetching %s", endpoint)
		}
		data, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return true, errors.Wrap(err, "reading response body")
		}
		return false, nil
	}, awsClientImplRetries, awsClientImplStartPeriod)
	if err != nil {
		return errors.WithStack(err)
	}

	matches := ebsRegex.FindSubmatch(data)
	if len(matches) < 2 {
		return errors.Errorf("could not find price JSON in response from %v", endpoint)
	}
	// define a one-off type for storing results from the price JSON
	prices := struct {
		Config struct {
			Regions []struct {
				Region string
				Types  []struct {
					Name   string
					Values []struct {
						Prices struct {
							USD string
						}
					}
				}
			}
		}
	}{}
	err = json.Unmarshal(matches[1], &prices)
	if err != nil {
		return errors.Wrap(err, "parsing price JSON")
	}

	pricePerRegion := map[string]float64{}
	for _, r := range prices.Config.Regions {
		for _, t := range r.Types {
			// only cache "general purpose" pricing for now
			if strings.Contains(t.Name, "ebsGPSSD") {
				if len(t.Values) == 0 {
					continue
				}
				price, err := strconv.ParseFloat(t.Values[0].Prices.USD, 64)
				if err != nil {
					continue
				}
				pricePerRegion[r.Region] = price
			}
		}
	}
	// one final sanity check that we actually pulled information, which will alert
	// us if, say, Amazon changes the structure of their JSON
	if len(pricePerRegion) == 0 {
		return errors.Errorf("unable to parse prices from %v", endpoint)
	}
	cpf.ebsPrices = pricePerRegion
	return nil
}

// calculateSpotCost is a helper for fetching spot price history and computing the
// cost of a task across a host's billing cycles.
func (cpf *cachingPriceFetcher) calculateSpotCost(ctx context.Context, client AWSClient, i *ec2.Instance, os osType, t timeRange) (float64, error) {
	rates, err := cpf.describeHourlySpotPriceHistory(ctx, client, hourlySpotPriceHistoryInput{
		iType: *i.InstanceType,
		zone:  *i.Placement.AvailabilityZone,
		os:    os,
		start: *i.LaunchTime,
		end:   t.end,
	})
	if err != nil {
		return 0, err
	}
	return spotCostForRange(t.start, t.end, rates), nil
}

type hourlySpotPriceHistoryInput struct {
	iType string
	zone  string
	os    osType
	start time.Time
	end   time.Time
}

// describeHourlySpotPriceHistory talks to Amazon to get spot price history, then
// simplifies that history into hourly billing rates starting from the supplied
// start time. Returns a slice of hour-separated spot prices or any errors that occur.
func (cpf *cachingPriceFetcher) describeHourlySpotPriceHistory(ctx context.Context, client AWSClient, input hourlySpotPriceHistoryInput) ([]spotRate, error) {
	// expand times to contain the full runtime of the host
	startFilter, endFilter := input.start.Add(-time.Hour), input.end.Add(time.Hour)
	osStr := string(input.os)
	filter := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []*string{&input.iType},
		ProductDescriptions: []*string{&osStr},
		AvailabilityZone:    &input.zone,
		StartTime:           &startFilter,
		EndTime:             &endFilter,
	}
	// iterate through all pages of results (the helper that does this for us appears to be broken)
	history := []*ec2.SpotPrice{}
	for {
		h, err := client.DescribeSpotPriceHistory(ctx, filter)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		history = append(history, h.SpotPriceHistory...)
		if h.NextToken != nil && *h.NextToken != "" {
			filter.NextToken = h.NextToken
		} else {
			break
		}
	}
	// this loop samples the spot price history (which includes updates for every few minutes)
	// into hourly billing periods. The price we are billed for an hour of spot time is the
	// current price at the start of the hour. Amazon returns spot price history sorted in
	// decreasing time order. We iterate backwards through the list to
	// pretend the ordering to increasing time.
	prices := []spotRate{}
	i := len(history) - 1
	for i >= 0 {
		// add the current hourly price if we're in the last result bucket
		// OR our billing hour starts the same time as the data (very rare)
		// OR our billing hour starts after the current bucket but before the next one
		if i == 0 || input.start.Equal(*history[i].Timestamp) ||
			input.start.After(*history[i].Timestamp) && input.start.Before(*history[i-1].Timestamp) {
			price, err := strconv.ParseFloat(*history[i].SpotPrice, 64)
			if err != nil {
				return nil, errors.Wrap(err, "parsing spot price")
			}
			prices = append(prices, spotRate{Time: input.start, Price: price})
			// we increment the hour but stay on the same price history index
			// in case the current spot price spans more than one hour
			input.start = input.start.Add(time.Hour)
			if input.start.After(input.end) {
				break
			}
		} else {
			// continue iterating through our price history whenever we
			// aren't matching the next billing hour
			i--
		}
	}
	return prices, nil
}

func getOsName(h *host.Host) osType {
	if strings.Contains(h.Distro.Arch, "windows") {
		return osWindows
	}
	return osLinux
}

// spotCostForRange determines the price of a range of spot price history.
// The hostRates parameter is expected to be a slice of (time, price) pairs
// representing every hour billing cycle. The function iterates through billing
// cycles, adding up the total cost of the time span across them.
//
// This problem, incidentally, may be a good algorithms interview question ;)
func spotCostForRange(start, end time.Time, rates []spotRate) float64 {
	cost := 0.0
	cur := start
	// this loop adds up the cost of a task over all the billing periods
	// it ran within.
	for i := range rates {
		// if our start time is after the current billing range, keep skipping
		// ahead until we find the starting range.
		if i+1 < len(rates) && cur.After(rates[i+1].Time) {
			continue
		}
		// if the task's end happens before the end of this billing period,
		// we only want to calculate the cost between the billing start
		// and task end, then exit; we also do this if we're in the last rate bucket.
		if i+1 == len(rates) || end.Before(rates[i+1].Time) {
			cost += end.Sub(cur).Hours() * rates[i].Price
			break
		}
		// in the default case, we get the duration between our current time
		// and the next billing period, and multiply that duration by the current price.
		cost += rates[i+1].Time.Sub(cur).Hours() * rates[i].Price
		cur = rates[i+1].Time
	}
	return cost
}
