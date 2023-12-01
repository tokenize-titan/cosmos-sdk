package keeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cosmossdk.io/collections"
	storetypes "cosmossdk.io/core/store"
	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/log"
	"cosmossdk.io/math"
	distrtypes "cosmossdk.io/x/distribution/types"
	"cosmossdk.io/x/protocolpool/types"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

type Keeper struct {
	storeService storetypes.KVStoreService
	authKeeper   types.AccountKeeper
	bankKeeper   types.BankKeeper

	cdc codec.BinaryCodec

	authority string

	// Global variables
	toDistribute uint64

	// State
	Schema         collections.Schema
	BudgetProposal collections.Map[sdk.AccAddress, types.Budget]
	ContinuousFund collections.Map[sdk.AccAddress, types.ContinuousFund]
	// RecipientFundPercentage key: RecipientAddr | value: Percentage in uint64
	RecipientFundPercentage collections.Map[sdk.AccAddress, uint64]
	// RecipientFundDistribution key: RecipientAddr | value: Claimable amount
	RecipientFundDistribution collections.Map[sdk.AccAddress, uint64]
}

func NewKeeper(cdc codec.BinaryCodec, storeService storetypes.KVStoreService,
	ak types.AccountKeeper, bk types.BankKeeper, authority string,
) Keeper {
	// ensure pool module account is set
	if addr := ak.GetModuleAddress(types.ModuleName); addr == nil {
		panic(fmt.Sprintf("%s module account has not been set", types.ModuleName))
	}
	sb := collections.NewSchemaBuilder(storeService)

	keeper := Keeper{
		storeService:              storeService,
		authKeeper:                ak,
		bankKeeper:                bk,
		cdc:                       cdc,
		authority:                 authority,
		BudgetProposal:            collections.NewMap(sb, types.BudgetKey, "budget", sdk.AccAddressKey, codec.CollValue[types.Budget](cdc)),
		ContinuousFund:            collections.NewMap(sb, types.ContinuousFundKey, "continuous_fund", sdk.AccAddressKey, codec.CollValue[types.ContinuousFund](cdc)),
		RecipientFundPercentage:   collections.NewMap(sb, types.RecipientFundPercentageKey, "recipient_fund_percentage", sdk.AccAddressKey, collections.Uint64Value),
		RecipientFundDistribution: collections.NewMap(sb, types.RecipientFundDistributionKey, "recipient_fund_distribution", sdk.AccAddressKey, collections.Uint64Value),
	}

	schema, err := sb.Build()
	if err != nil {
		panic(err)
	}
	keeper.Schema = schema

	return keeper
}

// GetAuthority returns the x/protocolpool module's authority.
func (k Keeper) GetAuthority() string {
	return k.authority
}

// Logger returns a module-specific logger.
func (k Keeper) Logger(ctx context.Context) log.Logger {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	return sdkCtx.Logger().With(log.ModuleKey, "x/"+types.ModuleName)
}

// FundCommunityPool allows an account to directly fund the community fund pool.
func (k Keeper) FundCommunityPool(ctx context.Context, amount sdk.Coins, sender sdk.AccAddress) error {
	return k.bankKeeper.SendCoinsFromAccountToModule(ctx, sender, types.ModuleName, amount)
}

// DistributeFromFeePool distributes funds from the protocolpool module account to
// a receiver address.
func (k Keeper) DistributeFromFeePool(ctx context.Context, amount sdk.Coins, receiveAddr sdk.AccAddress) error {
	return k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, receiveAddr, amount)
}

// GetCommunityPool get the community pool balance.
func (k Keeper) GetCommunityPool(ctx context.Context) (sdk.Coins, error) {
	moduleAccount := k.authKeeper.GetModuleAccount(ctx, types.ModuleName)
	if moduleAccount == nil {
		return nil, errorsmod.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", moduleAccount)
	}
	return k.bankKeeper.GetAllBalances(ctx, moduleAccount.GetAddress()), nil
}

func (k Keeper) withdrawContinuousFund(ctx context.Context, recipient sdk.AccAddress) (amount sdk.Coin, err error) {
	err = k.iterateAndUpdateFundsDistribution(ctx)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("error while iterating all the continuous funds: %w", err)
	}
	// get allocated continuous fund
	fundsAllocated, err := k.getDistributedFunds(ctx, recipient)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("error while getting distributed funds: %w", err)
	}

	cf, err := k.ContinuousFund.Get(ctx, recipient)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return sdk.Coin{}, fmt.Errorf("no continuous fund found for recipient: %s", recipient.String())
		}
		return sdk.Coin{}, err
	}

	recipientBal := k.bankKeeper.GetAllBalances(ctx, recipient)
	denom := recipientBal.Denoms()[0]
	recipientAmount := recipientBal.AmountOf(denom).Uint64()
	totalRecipientBal := recipientAmount + fundsAllocated
	// check if the recipient account balance exceeds maxDistributedCapital after distribution
	if totalRecipientBal < cf.MaxDistributedCapital {
		// Distribute funds to the recipient from pool module account
		err := k.DistributeFromFeePool(ctx, sdk.NewCoins(sdk.NewCoin(denom, math.NewIntFromUint64(fundsAllocated))), recipient)
		if err != nil {
			return sdk.Coin{}, err
		}

		// decrement fundsAllocated from toDistribute
		k.toDistribute -= fundsAllocated
	}

	return amount, nil
}

func (k Keeper) iterateAndUpdateFundsDistribution(ctx context.Context) error {
	var totalPercentageToBeDistributed uint64
	err := k.RecipientFundPercentage.Walk(ctx, nil, func(key sdk.AccAddress, value uint64) (stop bool, err error) {
		totalPercentageToBeDistributed += value
		return false, nil
	})
	if err != nil {
		return err
	}
	if totalPercentageToBeDistributed > 100 {
		return fmt.Errorf("total funds percentage is greater than one")
	}

	poolMAcc := k.authKeeper.GetModuleAccount(ctx, types.ModuleName)

	distrMAcc := k.authKeeper.GetModuleAccount(ctx, distrtypes.ModuleName)
	distrBal := k.bankKeeper.GetAllBalances(ctx, distrMAcc.GetAddress())
	distrDecAmount := sdk.NewDecCoinsFromCoins(distrBal...)

	err = k.RecipientFundPercentage.Walk(ctx, nil, func(key sdk.AccAddress, value uint64) (stop bool, err error) {
		// Calculate the funds to be distributed based on the percentage
		distributionAmount := distrDecAmount.MulDec(math.LegacyNewDecWithPrec(int64(value), 2))
		denom := distributionAmount.GetDenomByIndex(0)
		distrAmount := distributionAmount.AmountOf(denom)
		distrCoins := distrAmount.TruncateInt().Uint64()

		// Send distribution funds to pool module account [i.e., poolMAcc = MAcc + toDistribute]
		err = k.bankKeeper.SendCoinsFromModuleToModule(ctx, distrMAcc.GetName(), poolMAcc.GetName(), distrBal)
		if err != nil {
			return false, err
		}
		// Add all the coins to be distributed to toDistribute
		k.toDistribute += distrCoins

		// Set funds to be claimed
		toClaim, err := k.RecipientFundDistribution.Get(ctx, key)
		if err != nil {
			return false, err
		}
		amount := toClaim + distrCoins
		err = k.RecipientFundDistribution.Set(ctx, key, amount)
		if err != nil {
			return false, err
		}
		return false, nil
	})
	return err
}

func (k Keeper) getDistributedFunds(ctx context.Context, recipient sdk.AccAddress) (amount uint64, err error) {
	amount, err = k.RecipientFundDistribution.Get(ctx, recipient)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return 0, fmt.Errorf("no recipient fund found for recipient: %s", recipient.String())
		}
		return 0, err
	}

	// set claimable to zero
	err = k.RecipientFundDistribution.Set(ctx, recipient, 0)
	if err != nil {
		return 0, err
	}
	return amount, nil
}

func (k Keeper) claimFunds(ctx context.Context, recipient sdk.AccAddress) (amount sdk.Coin, err error) {
	// get claimable funds from distribution info
	amount, err = k.getClaimableFunds(ctx, recipient)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("error getting claimable funds: %w", err)
	}

	// distribute amount from feepool
	err = k.DistributeFromFeePool(ctx, sdk.NewCoins(amount), recipient)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("error distributing from fee pool: %w", err)
	}

	return amount, nil
}

func (k Keeper) getClaimableFunds(ctx context.Context, recipient sdk.AccAddress) (amount sdk.Coin, err error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	budget, err := k.BudgetProposal.Get(ctx, recipient)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return sdk.Coin{}, fmt.Errorf("no budget found for recipient: %s", recipient.String())
		}
		return sdk.Coin{}, err
	}

	// check if the distribution is completed
	if budget.TranchesLeft == 0 && budget.ClaimedAmount != nil {
		// check that claimed amount is equal to total budget
		if budget.ClaimedAmount.Equal(budget.TotalBudget) {
			// remove the entry of budget ended recipient
			if err := k.BudgetProposal.Remove(ctx, recipient); err != nil {
				return sdk.Coin{}, err
			}
			// Return the end of the budget
			return sdk.Coin{}, fmt.Errorf("budget ended for recipient: %s", recipient.String())
		}
	}

	currentTime := sdkCtx.BlockTime()
	startTime := budget.StartTime

	// Check if the start time is reached
	if currentTime.Before(*startTime) {
		return sdk.Coin{}, fmt.Errorf("distribution has not started yet")
	}

	if budget.NextClaimFrom == nil || budget.NextClaimFrom.IsZero() {
		budget.NextClaimFrom = budget.StartTime
	}

	if budget.TranchesLeft == 0 && budget.ClaimedAmount == nil {
		budget.TranchesLeft = budget.Tranches
		zeroCoin := sdk.NewCoin(budget.TotalBudget.Denom, math.ZeroInt())
		budget.ClaimedAmount = &zeroCoin
	}

	return k.calculateClaimableFunds(ctx, recipient, budget, currentTime)
}

func (k Keeper) calculateClaimableFunds(ctx context.Context, recipient sdk.AccAddress, budget types.Budget, currentTime time.Time) (amount sdk.Coin, err error) {
	// Calculate the time elapsed since the last claim time
	timeElapsed := currentTime.Sub(*budget.NextClaimFrom)

	// Check the time elapsed has passed period length
	if timeElapsed < *budget.Period {
		return sdk.Coin{}, fmt.Errorf("budget period has not passed yet")
	}

	// Calculate how many periods have passed
	periodsPassed := int64(timeElapsed) / int64(*budget.Period)

	// Calculate the amount to distribute for all passed periods
	coinsToDistribute := math.NewInt(periodsPassed).Mul(budget.TotalBudget.Amount.QuoRaw(int64(budget.Tranches)))
	amount = sdk.NewCoin(budget.TotalBudget.Denom, coinsToDistribute)

	// update the budget's remaining tranches
	budget.TranchesLeft -= uint64(periodsPassed)

	// update the ClaimedAmount
	claimedAmount := budget.ClaimedAmount.Add(amount)
	budget.ClaimedAmount = &claimedAmount

	// Update the last claim time for the budget
	nextClaimFrom := budget.NextClaimFrom.Add(*budget.Period)
	budget.NextClaimFrom = &nextClaimFrom

	k.Logger(ctx).Debug(fmt.Sprintf("Processing budget for recipient: %s. Amount: %s", budget.RecipientAddress, coinsToDistribute.String()))

	// Save the updated budget in the state
	if err := k.BudgetProposal.Set(ctx, recipient, budget); err != nil {
		return sdk.Coin{}, fmt.Errorf("error while updating the budget for recipient %s", budget.RecipientAddress)
	}

	return amount, nil
}

func (k Keeper) validateAndUpdateBudgetProposal(ctx context.Context, bp types.MsgSubmitBudgetProposal) (*types.Budget, error) {
	if bp.TotalBudget.IsZero() {
		return nil, fmt.Errorf("invalid budget proposal: total budget cannot be zero")
	}

	if err := validateAmount(sdk.NewCoins(*bp.TotalBudget)); err != nil {
		return nil, fmt.Errorf("invalid budget proposal: %w", err)
	}

	currentTime := sdk.UnwrapSDKContext(ctx).BlockTime()
	if bp.StartTime.IsZero() || bp.StartTime == nil {
		bp.StartTime = &currentTime
	}

	if currentTime.After(*bp.StartTime) {
		return nil, fmt.Errorf("invalid budget proposal: start time cannot be less than the current block time")
	}

	if bp.Tranches == 0 {
		return nil, fmt.Errorf("invalid budget proposal: tranches must be greater than zero")
	}

	if bp.Period == nil || *bp.Period == 0 {
		return nil, fmt.Errorf("invalid budget proposal: period length should be greater than zero")
	}

	// Create and return an updated budget proposal
	updatedBudget := types.Budget{
		RecipientAddress: bp.RecipientAddress,
		TotalBudget:      bp.TotalBudget,
		StartTime:        bp.StartTime,
		Tranches:         bp.Tranches,
		Period:           bp.Period,
	}

	return &updatedBudget, nil
}

// validateContinuousFund validates the fields of the CreateContinuousFund message.
func (k Keeper) validateContinuousFund(ctx context.Context, msg types.MsgCreateContinuousFund) error {
	// Validate percentage
	if msg.Percentage.IsZero() || msg.Percentage.IsNil() {
		return fmt.Errorf("percentage cannot be zero or empty")
	}
	if msg.Percentage.IsNegative() {
		return fmt.Errorf("percentage cannot be negative")
	}
	if msg.Percentage.GTE(math.LegacyOneDec()) {
		return fmt.Errorf("percentage cannot be greater than or equal to one")
	}

	// Validate maxDistributedCapital
	if msg.MaxDistributedCapital == 0 {
		return fmt.Errorf("invalid MaxDistributedCapital: amount cannot be zero")
	}

	// Validate expiry
	currentTime := sdk.UnwrapSDKContext(ctx).BlockTime()
	if msg.Expiry != nil {
		if msg.Expiry.Compare(currentTime) == -1 {
			return fmt.Errorf("expiry time cannot be less than the current block time")
		}
	}

	return nil
}
