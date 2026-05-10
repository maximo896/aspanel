package tencent

type Settings struct {
	SecretID           string
	SecretKey          string
	MaxPriceUSDPerHour float64
	HourlyBudgetUSD    float64
	BudgetHours        int
	InstanceType       string
	ImageID            string
	KeyID              string
	SecurityGroupID    string
	VpcID              string
	SubnetID           string
}

type SpotOffer struct {
	Region       string
	Zone         string
	InstanceType string
	PriceUSD     float64
	CPU          int
	MemoryGB     int
}

type LaunchRequest struct {
	Region       string
	Zone         string
	InstanceType string
	ImageID      string
	MaxPriceUSD  float64
	Count        int64
	UserDataB64  string
	KeyID        string
	Password     string
	SecurityIDs  []string
	VpcID        string
	SubnetID     string
}

type Instance struct {
	InstanceID   string
	Region       string
	Zone         string
	Status       string
	InstanceType string
	CPU          int
	MemoryGB     int
}
