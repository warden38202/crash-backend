package game;

import (
	"time"
	"math"
	"encoding/json"

	"slices"
	"errors"

	"math/big"
	"crypto/rand"

	"log/slog"

	"database/sql"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/zishang520/socket.io/v2/socket"

	"github.com/samott/crash-backend/config"
);

var (
	ErrPlayerAlreadyJoined = errors.New("player already joined game")
	ErrWrongGameState = errors.New("action invalid for current game state")
	ErrPlayerNotWaiting = errors.New("player not in waiting list")
	ErrAlreadyCashedOut = errors.New("player already cashed out")
)

const WAIT_TIME_SECS = 5;

const (
	GAMESTATE_STOPPED = iota;
	GAMESTATE_WAITING = iota;
	GAMESTATE_RUNNING = iota;
	GAMESTATE_CRASHED = iota;
	GAMESTATE_INVALID = iota;
);

const (
	EVENT_GAME_WAITING = "GameWaiting";
	EVENT_GAME_RUNNING = "GameRunning";
	EVENT_GAME_CRASHED = "GameCrashed";
	EVENT_PLAYER_WON   = "PlayerWon";
	EVENT_PLAYER_LOST  = "PlayerLost";
);

type Bank interface {
	IncreaseBalance(
		string,
		string,
		decimal.Decimal,
		string,
		uuid.UUID,
	) (decimal.Decimal, error);

	DecreaseBalance(
		string,
		string,
		decimal.Decimal,
		string,
		uuid.UUID,
	) (decimal.Decimal, error);

	GetBalance(string, string) (decimal.Decimal, error);

	GetBalances(wallet string) (map[string]decimal.Decimal, error);
};

type CashOut struct {
	absTime time.Time;
	duration time.Duration;
	multiplier decimal.Decimal;
	cashedOut bool;
	auto bool;
	payout decimal.Decimal
};

type Player struct {
	betAmount decimal.Decimal;
	currency string;
	autoCashOut decimal.Decimal;
	cashOut CashOut;
	wallet string;
	clientId socket.SocketId;
	timeOut *time.Timer;
};

type Observer struct {
	wallet string;
	socket *socket.Socket;
};

type Game struct {
	id uuid.UUID;
	state uint;
	players []*Player;
	waiting []*Player;
	observers map[socket.SocketId]*Observer;
	io *socket.Server;
	db *sql.DB;
	config *config.CrashConfig;
	bank Bank;
	startTime time.Time;
	endTime time.Time;
	duration time.Duration;
};

type CrashedGame struct {
	id uuid.UUID;
	startTime time.Time;
	duration time.Duration;
	multiplier decimal.Decimal;
	players int;
	winners int;
}

func (p *Player) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"betAmount"  : p.betAmount.String(),
		"currency"   : p.currency,
		"autoCashOut": p.currency,
		"wallet"     : p.wallet,
	});
}

func (g *CrashedGame) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id"         : g.id.String(),
		"startTime"  : g.startTime.Unix(),
		"duration"   : g.duration.Milliseconds(),
		"multiplier" : g.multiplier.String(),
		"players"    : g.players,
		"winners"    : g.winners,
	});
}

func NewGame(
	io *socket.Server,
	db *sql.DB,
	config *config.CrashConfig,
	bank Bank,
) (*Game, error) {
	gameId, err := uuid.NewV7();

	if err != nil {
		return nil, err;
	}

	return &Game{
		id: gameId,
		io: io,
		db: db,
		config: config,
		bank: bank,
		observers: make(map[socket.SocketId]*Observer),
		players: make([]*Player, 0),
		waiting: make([]*Player, 0),
	}, nil;
}

func (game *Game) GetConfig() (*config.CrashConfig) {
	return game.config;
}

func (game *Game) createNewGame() {
	randInt, err := rand.Int(rand.Reader, big.NewInt(10));

	if err != nil {
		return;
	}

	gameId, err := uuid.NewV7();

	if err != nil {
		return;
	}

	game.id = gameId;
	game.state = GAMESTATE_WAITING;

	untilStart := time.Second * WAIT_TIME_SECS;
	game.startTime = time.Now().Add(untilStart);
	game.duration = time.Duration(time.Second * time.Duration(randInt.Int64()));
	game.endTime = game.startTime.Add(game.duration);

	time.AfterFunc(untilStart, game.handleGameStart);
	time.AfterFunc(untilStart + game.duration, game.handleGameCrash);

	slog.Info(
		"Created new game",
		"game",
		game.id,
		"startTime",
		game.startTime,
		"endTime",
		game.endTime,
	);

	game.Emit(EVENT_GAME_WAITING, map[string]any{
		"startTime": game.startTime.Unix(),
	});
}

func (game *Game) handleGameStart() {
	slog.Info("Preparing to start game...", "game", game.id);

	if len(game.observers) == 0 {
		slog.Info("No observers; not starting.");
		game.state = GAMESTATE_STOPPED;
		return;
	}

	slog.Info("Starting game...", "game", game.id, "duration", game.duration);

	game.state = GAMESTATE_RUNNING;

	makeCallback := func(player *Player) func() {
		return func() {
			slog.Info("Auto cashing out", "wallet", player.wallet);
			game.handleCashOut(player.wallet, true);
		}
	};

	for i := range(game.players) {
		if !game.players[i].autoCashOut.Equal(decimal.Zero) {
			autoCashOut, _ := game.players[i].autoCashOut.Float64();
			timeOut := time.Duration(float64(time.Millisecond) * math.Log(autoCashOut) / 6E-5);
			game.players[i].timeOut = time.AfterFunc(timeOut, makeCallback(game.players[i]));
		}
	}

	game.Emit(EVENT_GAME_RUNNING, map[string]any{
		"startTime": game.startTime.Unix(),
	});
}

func (game *Game) handleGameCrash() {
	slog.Info("Crashing game...", "game", game.id);

	game.state = GAMESTATE_CRASHED;

	for i := range(game.players) {
		game.Emit(EVENT_PLAYER_LOST, map[string]any{
			"wallet": game.players[i].wallet,
		});
	}

	record, err := game.saveRecord();

	if err != nil {
		slog.Error("Error saving game record", "err", err);
		game.clearTimers();
		return;
	}

	slog.Info("Entering game wait state...");

	game.clearTimers();
	game.commitWaiting();

	game.Emit(EVENT_GAME_CRASHED, map[string]*CrashedGame{
		"game": record,
	});

	time.AfterFunc(WAIT_TIME_SECS * time.Second, game.createNewGame);
}

func (game *Game) HandlePlaceBet(
	client *socket.Socket,
	wallet string,
	currency string,
	betAmount decimal.Decimal,
	autoCashOut decimal.Decimal,
) error {
	player := Player{
		wallet: wallet,
		betAmount: betAmount,
		currency: currency,
		autoCashOut: autoCashOut,
		clientId: client.Id(),
	};

	for i := range(game.players) {
		if game.players[i].wallet == wallet {
			slog.Warn("Player already joined game");
			return ErrPlayerAlreadyJoined;
		}
	}

	bal, err := game.bank.GetBalance(
		wallet,
		currency,
	);

	if err != nil {
		slog.Warn("Failed to determine user balance", "err", err);
		return err;
	}

	if bal.LessThan(betAmount) {
		slog.Warn(
			"Insufficient balance for operation",
			"betAmount",
			betAmount,
			"balance",
			bal,
			"currency",
			currency,
		);

		return err;
	}

	if game.state == GAMESTATE_WAITING {
		game.players = append(game.players, &player);
	} else if (game.state == GAMESTATE_RUNNING) {
		game.waiting = append(game.waiting, &player);
	} else {
		return ErrWrongGameState;
	}

	game.Emit("BetList", map[string]any{
		"players": game.players,
		"waiting": game.waiting,
	});

	return nil;
}

func (game *Game) HandleCancelBet(wallet string) error {
	playerIndex := slices.IndexFunc(game.players, func(p *Player) bool {
		return p.wallet == wallet;
	});

	if playerIndex == -1 {
		return ErrPlayerNotWaiting;
	}

	game.players = slices.Delete(game.players, playerIndex, playerIndex + 1);

	return nil;
}

func (game *Game) HandleCashOut(wallet string) error {
	return game.handleCashOut(wallet, false);
}

func (game *Game) handleCashOut(wallet string, auto bool) error {
	if game.state != GAMESTATE_RUNNING {
		return ErrWrongGameState;
	}

	playerIndex := slices.IndexFunc(game.players, func(p *Player) bool {
		return p.wallet == wallet;
	});

	if playerIndex == -1 {
		return ErrPlayerNotWaiting;
	}

	player := game.players[playerIndex];

	if player.cashOut.cashedOut {
		return ErrAlreadyCashedOut;
	}

	timeNow := time.Now();
	duration := timeNow.Sub(game.startTime);

	payout, multiplier := game.calculatePayout(
		duration,
		player.betAmount,
	);

	slog.Info("Player cashed out", "wallet", player.wallet, "payout", payout, "currency", player.currency);

	player.cashOut = CashOut{
		absTime: timeNow,
		duration: duration,
		multiplier: multiplier,
		payout: payout,
		cashedOut: true,
		auto: auto,
	};

	var reason string;

	if (auto) {
		reason = "Auto cashout";
	} else {
		reason = "Cashout";
	}

	newBalance, err := game.bank.IncreaseBalance(
		player.wallet,
		player.currency,
		payout,
		reason,
		game.id,
	);

	if err != nil {
		slog.Error(
			"Failed to credit win",
			"wallet",
			player.wallet,
			"payout",
			payout,
			"currency",
			player.currency,
		);
	}

	observer, ok := game.observers[player.clientId];

	if ok && observer.socket.Connected() {
		observer.socket.Emit("balanceUpdate", map[string]string{
			"currency": player.currency,
			"balance" : newBalance.String(),
		});
	}

	game.Emit("BetList", map[string]any{
		"players": game.players,
		"waiting": game.waiting,
	});

	return nil;
}

func (game *Game) HandleConnect(client *socket.Socket) {
	_, exists := game.observers[client.Id()];

	if exists {
		return;
	}

	observer := Observer{
		wallet: "",
		socket: client,
	};

	game.observers[client.Id()] = &observer;

	if game.state == GAMESTATE_STOPPED {
		slog.Info("Entering game wait state...");
		game.createNewGame();

		return;
	}

	if game.state == GAMESTATE_WAITING {
		observer.socket.Emit(EVENT_GAME_WAITING, map[string]any{
			"startTime": game.startTime.Unix(),
		});

		return;
	}
}

func (game *Game) HandleLogin(client *socket.Socket, wallet string) {
	observer, exists := game.observers[client.Id()];

	if !exists {
		return;
	}

	observer.wallet = wallet;

	balances, err := game.bank.GetBalances(wallet);

	if err != nil {
		return;
	}

	observer.socket.Emit("balanceInit", map[string]map[string]decimal.Decimal{
		"balances" : balances,
	});
}

func (game *Game) HandleDisconnect(client *socket.Socket) {
	_, exists := game.observers[client.Id()];

	if !exists {
		return;
	}

	delete(game.observers, client.Id());
}

func (game *Game) clearTimers() {
	for i := range(game.players) {
		if game.players[i].timeOut != nil {
			game.players[i].timeOut.Stop();
			game.players[i].timeOut = nil;
		}
	}
}

func (game *Game) commitWaiting() {
	game.players = []*Player{};

	for i := range(game.waiting) {
		_, err := game.bank.DecreaseBalance(
			game.waiting[i].wallet,
			game.waiting[i].currency,
			game.waiting[i].betAmount,
			"Bet placed",
			game.id,
		);

		if err != nil {
			slog.Warn(
				"Unable to take balance for user; removing from game...",
				"wallet",
				game.waiting[i].wallet,
			);
			continue;
		}

		game.players = append(game.players, game.waiting[i]);
	}

	game.waiting = []*Player{};
}

func (game *Game) calculatePayout(
	duration time.Duration,
	betAmount decimal.Decimal,
) (decimal.Decimal, decimal.Decimal) {
	durationMs := decimal.NewFromInt(duration.Milliseconds());
	coeff := decimal.NewFromFloat(6E-5);
	e := decimal.NewFromFloat(math.Exp(1));
	multiplier := e.Pow(coeff.Mul(durationMs)).Truncate(2);

	return betAmount.Mul(multiplier), multiplier;
}

func (game *Game) calculateFinalMultiplier() (decimal.Decimal) {
	duration := game.endTime.Sub(game.startTime);
	durationMs := decimal.NewFromInt(duration.Milliseconds());
	coeff := decimal.NewFromFloat(6E-5);
	e := decimal.NewFromFloat(math.Exp(1));
	multiplier := e.Pow(coeff.Mul(durationMs)).Truncate(2);
	return multiplier;
}

func (game *Game) getRecentGames(limit int) ([]CrashedGame, error) {
	var games []CrashedGame;

	rows, err := game.db.Query(`
		SELECT id, startTime, (endTime - startTime) AS duration,
		multiplier, playerCount, winnerCount
		FROM games
		ORDER BY created DESC
		LIMIT ?
	`, limit);

	for rows.Next() {
		var gameRow CrashedGame;

		rows.Scan(
			gameRow.id,
			gameRow.startTime,
			gameRow.duration,
			gameRow.multiplier,
			gameRow.players,
			gameRow.winners,
		);

		games = append(games, gameRow);
	}

	if err != nil {
		return nil, err;
	}

	return games, nil;
}

func (game *Game) saveRecord() (*CrashedGame, error) {
	winners := 0;
	players := len(game.players);

	for i := range(game.players) {
		if game.players[i].cashOut.cashedOut {
			winners++;
		}
	}

	multiplier := game.calculateFinalMultiplier();

	_, err := game.db.Exec(`
		INSERT INTO games
		(id, startTime, endTime, multiplier, playerCount, winnerCount)
		VALUES
		(?, ?, ?, ?, ?, ?)
	`, game.id, game.startTime, game.endTime, multiplier,
		players, winners);

	if err != nil {
		return nil, err;
	}

	record := CrashedGame{
		id: game.id,
		startTime: game.startTime,
		duration: game.endTime.Sub(game.startTime),
		multiplier: multiplier,
		players: players,
		winners: winners,
	};

	return &record, nil;
}

/**
 * Temporary hack until I can figure out why io.Emit()
 * isn't working.
 */
func (game *Game) Emit(ev string, params ...any) {
	for _, observer := range game.observers {
		observer.socket.Emit(ev, params...);
	}
}
