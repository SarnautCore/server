package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/account"
	"github.com/SarnautCore/server/internal/account/secret"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
)

func main() {
	address := flag.String("address", "127.0.0.1:4242", "shard QUIC address")
	zoneID := flag.String("zone", "InstLeague1", "zone to enter")
	duration := flag.Duration("duration", 5*time.Second, "probe duration")
	packID := flag.String("pack", "", "runtime pack digest to claim in the hello")
	ticket := flag.String("ticket", "", "shard ticket to present on enter zone")
	authURL := flag.String("auth", "", "auth service base URL; when set, the probe registers, logs in and mints its own ticket")
	email := flag.String("email", "probe@example.invalid", "account to register or log in as, with -auth")
	password := flag.String("password", "probe-password", "account password, with -auth")
	character := flag.String("character", "Probeling", "character to use or create, with -auth")
	expectRefusal := flag.Bool("expect-refusal", false, "succeed only if the shard refuses admission; for proving an unauthenticated connection cannot enter a zone")
	flag.Parse()

	options := probeOptions{
		address:       *address,
		zoneID:        *zoneID,
		packID:        *packID,
		ticket:        *ticket,
		authURL:       *authURL,
		email:         *email,
		password:      *password,
		character:     *character,
		expectRefusal: *expectRefusal,
		duration:      *duration,
	}
	if err := run(options); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(1)
	}
}

// probeOptions is what one probe run needs. It is a struct because the flag
// list crossed the point where positional arguments stop being readable.
type probeOptions struct {
	address       string
	zoneID        string
	packID        string
	ticket        string
	authURL       string
	email         string
	password      string
	character     string
	expectRefusal bool
	duration      time.Duration
}

func run(options probeOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), options.duration)
	defer cancel()

	address, zoneID, packID := options.address, options.zoneID, options.packID
	ticket := options.ticket
	// The shard admits nobody without a ticket (ADR 0030), and a ticket is
	// minted out of band. The probe does that flow itself so a smoke run is one
	// command rather than a shell pipeline of curl calls.
	if ticket == "" && options.authURL != "" {
		client := account.Client{BaseURL: options.authURL}
		minted, characterID, err := client.EnsureTicket(
			ctx,
			secret.New(options.email),
			secret.New(options.password),
			options.character,
		)
		if err != nil {
			return err
		}
		fmt.Printf("authenticated character=%s id=%s\n", options.character, characterID)
		ticket = minted.Reveal()
	}

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "probe",
		PackID:          packID,
		Ticket:          ticket,
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		return err
	}
	entered, err := client.EnterZone(connection, zoneID)
	if options.expectRefusal {
		// The point of this mode is that admission fails. Reporting the refusal
		// as success is what lets a script prove the shard is closed rather
		// than merely observing that something went wrong.
		if err == nil {
			return fmt.Errorf("the shard admitted a connection with no valid ticket")
		}
		fmt.Printf("refused as expected: %v\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	// The spawn is printed because it is an assertion a script can make: the
	// server's answer is authoritative, and on a first login it is the chargen
	// option's spawn rather than the zone's configured one (ADR 0032).
	fmt.Printf(
		"entered zone=%s entity=%d spawn=%g,%g,%g datagrams=%t server_pack=%q\n",
		entered.GetZoneId(),
		entered.GetOwnEntityId(),
		entered.GetSpawnPosition().GetX(),
		entered.GetSpawnPosition().GetY(),
		entered.GetSpawnPosition().GetZ(),
		connection.SupportsUnreliable(),
		hello.GetPackId(),
	)

	go sendMovement(ctx, client, connection)
	var snapshots, entities, named int
	var lastTick uint64
	for ctx.Err() == nil {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		snapshots++
		entities += len(snapshot.GetEntities())
		lastTick = snapshot.GetServerTick()
		for _, entity := range snapshot.GetEntities() {
			if entity.GetContentId() != "" {
				named++
			}
		}
	}
	fmt.Printf(
		"snapshots=%d entity_records=%d content_identified=%d last_server_tick=%d\n",
		snapshots,
		entities,
		named,
		lastTick,
	)
	// A clean exit, so the shard's teardown runs ahead of the disconnect rather
	// than racing it.
	if err := client.Logout(connection); err != nil {
		return err
	}
	return nil
}

func sendMovement(ctx context.Context, client session.Client, connection transport.Connection) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sequence++
			_ = client.SendMoveIntent(connection, &sarnautv1.ClientMoveIntent{
				Seq:       sequence,
				Input:     &sarnautv1.Vec3{X: 1},
				DtSeconds: 0.15,
			})
		}
	}
}
