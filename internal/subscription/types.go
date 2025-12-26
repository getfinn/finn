package subscription

// SubscriptionTier represents the user's subscription level
type SubscriptionTier string

const (
	TierStandard SubscriptionTier = "standard"
	TierPro      SubscriptionTier = "pro"
	TierMax      SubscriptionTier = "max"
)

// Subscription holds user subscription information
type Subscription struct {
	Tier       SubscriptionTier `json:"tier"`
	MaxFolders int              `json:"max_folders"`
	Active     bool             `json:"active"`
}

// GetMaxFolders returns the maximum number of folders allowed for a tier
func GetMaxFolders(tier SubscriptionTier) int {
	switch tier {
	case TierStandard:
		return 5
	case TierPro:
		return 10
	case TierMax:
		return 20
	default:
		return 5 // Default to standard
	}
}

// NewSubscription creates a subscription with the correct folder limits
func NewSubscription(tier SubscriptionTier) *Subscription {
	return &Subscription{
		Tier:       tier,
		MaxFolders: GetMaxFolders(tier),
		Active:     true,
	}
}

// CanAddFolder checks if the user can add another folder
func (s *Subscription) CanAddFolder(currentFolderCount int) bool {
	if !s.Active {
		return false
	}
	return currentFolderCount < s.MaxFolders
}
