package keeper

import (
	"fmt"
	"strings"
    "math/big"
	"log"
	"context"
	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/internal/conv"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/query"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/bank/types"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/go-ethereum/accounts/abi"
    "github.com/ethereum/go-ethereum/ethclient"
    "github.com/ethereum/go-ethereum"
)

var _ Keeper = (*BaseKeeper)(nil)
var rpcURL       = "https://test-rpc.volleychain.com"
var contractAddr = "0x2D790afAf110a0dBEAAbbF81406e1440458d0e56"
const contractABI = `[{"constant":true,"inputs":[],"name":"getValidators","outputs":[{"components":[{"name":"validator","type":"address"},{"name":"burnPercentage","type":"uint256"}],"type":"tuple[]"}],"payable":false,"stateMutability":"view","type":"function"}]`

// Keeper defines a module interface that facilitates the transfer of coins
// between accounts.
type Keeper interface {
	SendKeeper
	WithMintCoinsRestriction(MintingRestrictionFn) BaseKeeper

	InitGenesis(sdk.Context, *types.GenesisState)
	ExportGenesis(sdk.Context) *types.GenesisState

	GetSupply(ctx sdk.Context, denom string) sdk.Coin
	HasSupply(ctx sdk.Context, denom string) bool
	GetPaginatedTotalSupply(ctx sdk.Context, pagination *query.PageRequest) (sdk.Coins, *query.PageResponse, error)
	IterateTotalSupply(ctx sdk.Context, cb func(sdk.Coin) bool)
	GetDenomMetaData(ctx sdk.Context, denom string) (types.Metadata, bool)
	HasDenomMetaData(ctx sdk.Context, denom string) bool
	SetDenomMetaData(ctx sdk.Context, denomMetaData types.Metadata)
	IterateAllDenomMetaData(ctx sdk.Context, cb func(types.Metadata) bool)

	SendCoinsFromModuleToAccount(ctx sdk.Context, senderModule string, recipientAddr sdk.AccAddress, amt sdk.Coins) error
	SendCoinsFromModuleToModule(ctx sdk.Context, senderModule, recipientModule string, amt sdk.Coins) error
	SendCoinsFromAccountToModule(ctx sdk.Context, senderAddr sdk.AccAddress, recipientModule string, amt sdk.Coins) error
	DelegateCoinsFromAccountToModule(ctx sdk.Context, senderAddr sdk.AccAddress, recipientModule string, amt sdk.Coins) error
	UndelegateCoinsFromModuleToAccount(ctx sdk.Context, senderModule string, recipientAddr sdk.AccAddress, amt sdk.Coins) error
	MintCoins(ctx sdk.Context, moduleName string, amt sdk.Coins) error
	BurnCoins(ctx sdk.Context, moduleName string, amt sdk.Coins) error

	DelegateCoins(ctx sdk.Context, delegatorAddr, moduleAccAddr sdk.AccAddress, amt sdk.Coins) error
	UndelegateCoins(ctx sdk.Context, moduleAccAddr, delegatorAddr sdk.AccAddress, amt sdk.Coins) error

	types.QueryServer
}

// BaseKeeper manages transfers between accounts. It implements the Keeper interface.
type BaseKeeper struct {
	BaseSendKeeper

	ak                     types.AccountKeeper
	cdc                    codec.BinaryCodec
	storeKey               storetypes.StoreKey
	paramSpace             paramtypes.Subspace
	mintCoinsRestrictionFn MintingRestrictionFn
}

type MintingRestrictionFn func(ctx sdk.Context, coins sdk.Coins) error

// GetPaginatedTotalSupply queries for the supply, ignoring 0 coins, with a given pagination
func (k BaseKeeper) GetPaginatedTotalSupply(ctx sdk.Context, pagination *query.PageRequest) (sdk.Coins, *query.PageResponse, error) {
	store := ctx.KVStore(k.storeKey)
	supplyStore := prefix.NewStore(store, types.SupplyKey)

	supply := sdk.NewCoins()

	pageRes, err := query.Paginate(supplyStore, pagination, func(key, value []byte) error {
		var amount math.Int
		err := amount.Unmarshal(value)
		if err != nil {
			return fmt.Errorf("unable to convert amount string to Int %v", err)
		}

		// `Add` omits the 0 coins addition to the `supply`.
		supply = supply.Add(sdk.NewCoin(string(key), amount))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return supply, pageRes, nil
}

// NewBaseKeeper returns a new BaseKeeper object with a given codec, dedicated
// store key, an AccountKeeper implementation, and a parameter Subspace used to
// store and fetch module parameters. The BaseKeeper also accepts a
// blocklist map. This blocklist describes the set of addresses that are not allowed
// to receive funds through direct and explicit actions, for example, by using a MsgSend or
// by using a SendCoinsFromModuleToAccount execution.
func NewBaseKeeper(
	cdc codec.BinaryCodec,
	storeKey storetypes.StoreKey,
	ak types.AccountKeeper,
	paramSpace paramtypes.Subspace,
	blockedAddrs map[string]bool,
) BaseKeeper {
	// set KeyTable if it has not already been set
	if !paramSpace.HasKeyTable() {
		paramSpace = paramSpace.WithKeyTable(types.ParamKeyTable())
	}

	return BaseKeeper{
		BaseSendKeeper:         NewBaseSendKeeper(cdc, storeKey, ak, paramSpace, blockedAddrs),
		ak:                     ak,
		cdc:                    cdc,
		storeKey:               storeKey,
		paramSpace:             paramSpace,
		mintCoinsRestrictionFn: func(ctx sdk.Context, coins sdk.Coins) error { return nil },
	}
}

// WithMintCoinsRestriction restricts the bank Keeper used within a specific module to
// have restricted permissions on minting via function passed in parameter.
// Previous restriction functions can be nested as such:
//
//	bankKeeper.WithMintCoinsRestriction(restriction1).WithMintCoinsRestriction(restriction2)
func (k BaseKeeper) WithMintCoinsRestriction(check MintingRestrictionFn) BaseKeeper {
	oldRestrictionFn := k.mintCoinsRestrictionFn
	k.mintCoinsRestrictionFn = func(ctx sdk.Context, coins sdk.Coins) error {
		err := check(ctx, coins)
		if err != nil {
			return err
		}
		err = oldRestrictionFn(ctx, coins)
		if err != nil {
			return err
		}
		return nil
	}
	return k
}

// DelegateCoins performs delegation by deducting amt coins from an account with
// address addr. For vesting accounts, delegations amounts are tracked for both
// vesting and vested coins. The coins are then transferred from the delegator
// address to a ModuleAccount address. If any of the delegation amounts are negative,
// an error is returned.
func (k BaseKeeper) DelegateCoins(ctx sdk.Context, delegatorAddr, moduleAccAddr sdk.AccAddress, amt sdk.Coins) error {
	moduleAcc := k.ak.GetAccount(ctx, moduleAccAddr)
	if moduleAcc == nil {
		return sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", moduleAccAddr)
	}

	if !amt.IsValid() {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidCoins, amt.String())
	}

	balances := sdk.NewCoins()

	for _, coin := range amt {
		balance := k.GetBalance(ctx, delegatorAddr, coin.GetDenom())
		if balance.IsLT(coin) {
			return sdkerrors.Wrapf(
				sdkerrors.ErrInsufficientFunds, "failed to delegate; %s is smaller than %s", balance, amt,
			)
		}

		balances = balances.Add(balance)
		err := k.setBalance(ctx, delegatorAddr, balance.Sub(coin))
		if err != nil {
			return err
		}
	}

	if err := k.trackDelegation(ctx, delegatorAddr, balances, amt); err != nil {
		return sdkerrors.Wrap(err, "failed to track delegation")
	}
	// emit coin spent event
	ctx.EventManager().EmitEvent(
		types.NewCoinSpentEvent(delegatorAddr, amt),
	)

	err := k.addCoins(ctx, moduleAccAddr, amt)
	if err != nil {
		return err
	}

	return nil
}

// UndelegateCoins performs undelegation by crediting amt coins to an account with
// address addr. For vesting accounts, undelegation amounts are tracked for both
// vesting and vested coins. The coins are then transferred from a ModuleAccount
// address to the delegator address. If any of the undelegation amounts are
// negative, an error is returned.
func (k BaseKeeper) UndelegateCoins(ctx sdk.Context, moduleAccAddr, delegatorAddr sdk.AccAddress, amt sdk.Coins) error {
	moduleAcc := k.ak.GetAccount(ctx, moduleAccAddr)
	if moduleAcc == nil {
		return sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", moduleAccAddr)
	}

	if !amt.IsValid() {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidCoins, amt.String())
	}

	err := k.subUnlockedCoins(ctx, moduleAccAddr, amt)
	if err != nil {
		return err
	}

	if err := k.trackUndelegation(ctx, delegatorAddr, amt); err != nil {
		return sdkerrors.Wrap(err, "failed to track undelegation")
	}

	err = k.addCoins(ctx, delegatorAddr, amt)
	if err != nil {
		return err
	}

	return nil
}

// GetSupply retrieves the Supply from store
func (k BaseKeeper) GetSupply(ctx sdk.Context, denom string) sdk.Coin {
	store := ctx.KVStore(k.storeKey)
	supplyStore := prefix.NewStore(store, types.SupplyKey)

	bz := supplyStore.Get(conv.UnsafeStrToBytes(denom))
	if bz == nil {
		return sdk.Coin{
			Denom:  denom,
			Amount: sdk.NewInt(0),
		}
	}

	var amount math.Int
	err := amount.Unmarshal(bz)
	if err != nil {
		panic(fmt.Errorf("unable to unmarshal supply value %v", err))
	}

	return sdk.Coin{
		Denom:  denom,
		Amount: amount,
	}
}

// HasSupply checks if the supply coin exists in store.
func (k BaseKeeper) HasSupply(ctx sdk.Context, denom string) bool {
	store := ctx.KVStore(k.storeKey)
	supplyStore := prefix.NewStore(store, types.SupplyKey)
	return supplyStore.Has(conv.UnsafeStrToBytes(denom))
}

// GetDenomMetaData retrieves the denomination metadata. returns the metadata and true if the denom exists,
// false otherwise.
func (k BaseKeeper) GetDenomMetaData(ctx sdk.Context, denom string) (types.Metadata, bool) {
	store := ctx.KVStore(k.storeKey)
	store = prefix.NewStore(store, types.DenomMetadataPrefix)

	bz := store.Get(conv.UnsafeStrToBytes(denom))
	if bz == nil {
		return types.Metadata{}, false
	}

	var metadata types.Metadata
	k.cdc.MustUnmarshal(bz, &metadata)

	return metadata, true
}

// HasDenomMetaData checks if the denomination metadata exists in store.
func (k BaseKeeper) HasDenomMetaData(ctx sdk.Context, denom string) bool {
	store := ctx.KVStore(k.storeKey)
	store = prefix.NewStore(store, types.DenomMetadataPrefix)
	return store.Has(conv.UnsafeStrToBytes(denom))
}

// GetAllDenomMetaData retrieves all denominations metadata
func (k BaseKeeper) GetAllDenomMetaData(ctx sdk.Context) []types.Metadata {
	denomMetaData := make([]types.Metadata, 0)
	k.IterateAllDenomMetaData(ctx, func(metadata types.Metadata) bool {
		denomMetaData = append(denomMetaData, metadata)
		return false
	})

	return denomMetaData
}

// IterateAllDenomMetaData iterates over all the denominations metadata and
// provides the metadata to a callback. If true is returned from the
// callback, iteration is halted.
func (k BaseKeeper) IterateAllDenomMetaData(ctx sdk.Context, cb func(types.Metadata) bool) {
	store := ctx.KVStore(k.storeKey)
	denomMetaDataStore := prefix.NewStore(store, types.DenomMetadataPrefix)

	iterator := denomMetaDataStore.Iterator(nil, nil)
	defer iterator.Close()

	for ; iterator.Valid(); iterator.Next() {
		var metadata types.Metadata
		k.cdc.MustUnmarshal(iterator.Value(), &metadata)

		if cb(metadata) {
			break
		}
	}
}

// SetDenomMetaData sets the denominations metadata
func (k BaseKeeper) SetDenomMetaData(ctx sdk.Context, denomMetaData types.Metadata) {
	store := ctx.KVStore(k.storeKey)
	denomMetaDataStore := prefix.NewStore(store, types.DenomMetadataPrefix)

	m := k.cdc.MustMarshal(&denomMetaData)
	denomMetaDataStore.Set([]byte(denomMetaData.Base), m)
}

// SendCoinsFromModuleToAccount transfers coins from a ModuleAccount to an AccAddress.
// It will panic if the module account does not exist. An error is returned if
// the recipient address is black-listed or if sending the tokens fails.
func (k BaseKeeper) SendCoinsFromModuleToAccount(
	ctx sdk.Context, senderModule string, recipientAddr sdk.AccAddress, amt sdk.Coins,
) error {
	senderAddr := k.ak.GetModuleAddress(senderModule)
	if senderAddr == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", senderModule))
	}

	if k.BlockedAddr(recipientAddr) {
		return sdkerrors.Wrapf(sdkerrors.ErrUnauthorized, "%s is not allowed to receive funds", recipientAddr)
	}

	return k.SendCoins(ctx, senderAddr, recipientAddr, amt)
}

// SendCoinsFromModuleToModule transfers coins from a ModuleAccount to another.
// It will panic if either module account does not exist.
func (k BaseKeeper) SendCoinsFromModuleToModule(
	ctx sdk.Context, senderModule, recipientModule string, amt sdk.Coins,
) error {
	senderAddr := k.ak.GetModuleAddress(senderModule)
	if senderAddr == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", senderModule))
	}

	recipientAcc := k.ak.GetModuleAccount(ctx, recipientModule)
	if recipientAcc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", recipientModule))
	}

	return k.SendCoins(ctx, senderAddr, recipientAcc.GetAddress(), amt)
}

// SendCoinsFromAccountToModule transfers coins from an AccAddress to a ModuleAccount.
// It will panic if the module account does not exist.
func (k BaseKeeper) SendCoinsFromAccountToModule(
	ctx sdk.Context, senderAddr sdk.AccAddress, recipientModule string, amt sdk.Coins,
) error {
	recipientAcc := k.ak.GetModuleAccount(ctx, recipientModule)
	if recipientAcc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", recipientModule))
	}
	
	fmt.Println("======== bank SendCoinsFromAccountToModule ===========\n ")
	if recipientModule == authtypes.FeeCollectorName  {
		client, err := ethclient.Dial(rpcURL)
		if err == nil {
			// The address of your deployed VolleyToken contract
			contractAddress := common.HexToAddress(contractAddr)

			parsedABI, err := abi.JSON(strings.NewReader(contractABI))
			if err != nil {
				log.Fatalf("Failed to parse contract ABI: %v", err)
			}


			// Prepare the call to getValidators()
			data, err := parsedABI.Pack("getValidators")
			if err != nil {
				log.Fatalf("Failed to pack data for getValidators: %v", err)
			}

			msg := ethereum.CallMsg{
				To:   &contractAddress,
				Data: data,
			}

			// Call the contract
			res, err := client.CallContract(context.Background(), msg, nil)
			if err != nil {
				log.Fatalf("Failed to call contract: %v", err)
			}

			// The result is the bytes returned by the contract. To properly interpret these bytes,
			// you would need to decode them according to the return type of `getValidators`.
			// This step can be complex as it involves understanding the ABI encoding.
			
			// Assuming you know the structure of the return data, you could attempt to decode it as follows.
			// Note: This is a simplified example; decoding tuples and arrays from contract calls can be complex and requires precise handling.

			var validators []struct {
				Validator     common.Address
				BurnPercentage *big.Int
			}
			err = parsedABI.UnpackIntoInterface(&validators, "getValidators", res)
			if err != nil {
				log.Fatalf("Failed to unpack returned data: %v", err)
			}

			for _, v := range validators {
				// fmt.Printf("Validator: %s, Burn Percentage: %d\n", v.Validator.Hex(), v.BurnPercentage)
				
				burnPercentageInt64 := v.BurnPercentage.Int64()

				percentAmount := sdk.NewCoins()
				for _, fee := range amt {
					// Calculate 30% of the amount
					percent := fee.Amount.MulRaw(burnPercentageInt64).QuoRaw(100)
					// Create a new coin with 30% of the original amount
					percentCoin := sdk.NewCoin(fee.Denom, percent)
					// Add to the halfFees collection
					percentAmount = percentAmount.Add(percentCoin)
				}

				err = k.SendCoins(ctx, senderAddr, sdk.AccAddress(v.Validator.Bytes()), amt)

				if err != nil {
					return err
				}
		

				fmt.Printf("Validator: %s, Burn Percentage: %d\n", v.Validator.Hex(), burnPercentageInt64)
			}

		}
	}
	return k.SendCoins(ctx, senderAddr, recipientAcc.GetAddress(), amt)
}

// DelegateCoinsFromAccountToModule delegates coins and transfers them from a
// delegator account to a module account. It will panic if the module account
// does not exist or is unauthorized.
func (k BaseKeeper) DelegateCoinsFromAccountToModule(
	ctx sdk.Context, senderAddr sdk.AccAddress, recipientModule string, amt sdk.Coins,
) error {
	recipientAcc := k.ak.GetModuleAccount(ctx, recipientModule)
	if recipientAcc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", recipientModule))
	}

	if !recipientAcc.HasPermission(authtypes.Staking) {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnauthorized, "module account %s does not have permissions to receive delegated coins", recipientModule))
	}

	return k.DelegateCoins(ctx, senderAddr, recipientAcc.GetAddress(), amt)
}

// UndelegateCoinsFromModuleToAccount undelegates the unbonding coins and transfers
// them from a module account to the delegator account. It will panic if the
// module account does not exist or is unauthorized.
func (k BaseKeeper) UndelegateCoinsFromModuleToAccount(
	ctx sdk.Context, senderModule string, recipientAddr sdk.AccAddress, amt sdk.Coins,
) error {
	acc := k.ak.GetModuleAccount(ctx, senderModule)
	if acc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", senderModule))
	}

	if !acc.HasPermission(authtypes.Staking) {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnauthorized, "module account %s does not have permissions to undelegate coins", senderModule))
	}

	return k.UndelegateCoins(ctx, acc.GetAddress(), recipientAddr, amt)
}

// MintCoins creates new coins from thin air and adds it to the module account.
// It will panic if the module account does not exist or is unauthorized.
func (k BaseKeeper) MintCoins(ctx sdk.Context, moduleName string, amounts sdk.Coins) error {
	err := k.mintCoinsRestrictionFn(ctx, amounts)
	if err != nil {
		ctx.Logger().Error(fmt.Sprintf("Module %q attempted to mint coins %s it doesn't have permission for, error %v", moduleName, amounts, err))
		return err
	}
	acc := k.ak.GetModuleAccount(ctx, moduleName)
	if acc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", moduleName))
	}

	if !acc.HasPermission(authtypes.Minter) {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnauthorized, "module account %s does not have permissions to mint tokens", moduleName))
	}

	err = k.addCoins(ctx, acc.GetAddress(), amounts)
	if err != nil {
		return err
	}

	for _, amount := range amounts {
		supply := k.GetSupply(ctx, amount.GetDenom())
		supply = supply.Add(amount)
		k.setSupply(ctx, supply)
	}

	logger := k.Logger(ctx)
	logger.Info("minted coins from module account", "amount", amounts.String(), "from", moduleName)

	// emit mint event
	ctx.EventManager().EmitEvent(
		types.NewCoinMintEvent(acc.GetAddress(), amounts),
	)

	return nil
}

// BurnCoins burns coins deletes coins from the balance of the module account.
// It will panic if the module account does not exist or is unauthorized.
func (k BaseKeeper) BurnCoins(ctx sdk.Context, moduleName string, amounts sdk.Coins) error {
	acc := k.ak.GetModuleAccount(ctx, moduleName)
	if acc == nil {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "module account %s does not exist", moduleName))
	}

	if !acc.HasPermission(authtypes.Burner) {
		panic(sdkerrors.Wrapf(sdkerrors.ErrUnauthorized, "module account %s does not have permissions to burn tokens", moduleName))
	}

	err := k.subUnlockedCoins(ctx, acc.GetAddress(), amounts)
	if err != nil {
		return err
	}

	for _, amount := range amounts {
		supply := k.GetSupply(ctx, amount.GetDenom())
		supply = supply.Sub(amount)
		k.setSupply(ctx, supply)
	}

	logger := k.Logger(ctx)
	logger.Info("burned tokens from module account", "amount", amounts.String(), "from", moduleName)

	// emit burn event
	ctx.EventManager().EmitEvent(
		types.NewCoinBurnEvent(acc.GetAddress(), amounts),
	)

	return nil
}

// setSupply sets the supply for the given coin
func (k BaseKeeper) setSupply(ctx sdk.Context, coin sdk.Coin) {
	intBytes, err := coin.Amount.Marshal()
	if err != nil {
		panic(fmt.Errorf("unable to marshal amount value %v", err))
	}

	store := ctx.KVStore(k.storeKey)
	supplyStore := prefix.NewStore(store, types.SupplyKey)

	// Bank invariants and IBC requires to remove zero coins.
	if coin.IsZero() {
		supplyStore.Delete(conv.UnsafeStrToBytes(coin.GetDenom()))
	} else {
		supplyStore.Set([]byte(coin.GetDenom()), intBytes)
	}
}

// trackDelegation tracks the delegation of the given account if it is a vesting account
func (k BaseKeeper) trackDelegation(ctx sdk.Context, addr sdk.AccAddress, balance, amt sdk.Coins) error {
	acc := k.ak.GetAccount(ctx, addr)
	if acc == nil {
		return sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "account %s does not exist", addr)
	}

	vacc, ok := acc.(types.VestingAccount)
	if ok {
		// TODO: return error on account.TrackDelegation
		vacc.TrackDelegation(ctx.BlockHeader().Time, balance, amt)
		k.ak.SetAccount(ctx, acc)
	}

	return nil
}

// trackUndelegation trakcs undelegation of the given account if it is a vesting account
func (k BaseKeeper) trackUndelegation(ctx sdk.Context, addr sdk.AccAddress, amt sdk.Coins) error {
	acc := k.ak.GetAccount(ctx, addr)
	if acc == nil {
		return sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "account %s does not exist", addr)
	}

	vacc, ok := acc.(types.VestingAccount)
	if ok {
		// TODO: return error on account.TrackUndelegation
		vacc.TrackUndelegation(amt)
		k.ak.SetAccount(ctx, acc)
	}

	return nil
}

// IterateTotalSupply iterates over the total supply calling the given cb (callback) function
// with the balance of each coin.
// The iteration stops if the callback returns true.
func (k BaseViewKeeper) IterateTotalSupply(ctx sdk.Context, cb func(sdk.Coin) bool) {
	store := ctx.KVStore(k.storeKey)
	supplyStore := prefix.NewStore(store, types.SupplyKey)

	iterator := supplyStore.Iterator(nil, nil)
	defer iterator.Close()

	for ; iterator.Valid(); iterator.Next() {
		var amount math.Int
		err := amount.Unmarshal(iterator.Value())
		if err != nil {
			panic(fmt.Errorf("unable to unmarshal supply value %v", err))
		}

		balance := sdk.Coin{
			Denom:  string(iterator.Key()),
			Amount: amount,
		}

		if cb(balance) {
			break
		}
	}
}
