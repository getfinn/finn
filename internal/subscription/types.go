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
// Returns -1 for unlimited (Max tier)
func GetMaxFolders(tier SubscriptionTier) int {
	switch tier {
	case TierStandard:
		return 3 // Free tier: 3 folders
	case TierPro:
		return 5 // Pro tier ($10/mo): 5 folders
	case TierMax:
		return -1 // Max tier ($25/mo): Unlimited folders
	default:
		return 3 // Default to free tier
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
// Returns true for unlimited (-1) tier
func (s *Subscription) CanAddFolder(currentFolderCount int) bool {
	if !s.Active {
		return false
	}
	// -1 means unlimited folders (Max tier)
	if s.MaxFolders == -1 {
		return true
	}
	return currentFolderCount < s.MaxFolders
}
