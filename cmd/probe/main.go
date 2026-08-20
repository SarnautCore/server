package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
)

func main() {
	address := flag.String("address", "127.0.0.1:4242", "shard QUIC address")
	zoneID := flag.String("zone", "InstLeague1", "zone to enter")
	duration := flag.Duration("duration", 5*time.Second, "probe duration")
	packID := flag.String("pack", "", "runtime pack digest to claim in the hello")
	ticket := flag.String("ticket", "", "shard ticket to present on enter zone")
	flag.Parse()

	if err := run(*address, *zoneID, *packID, *ticket, *duration); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(1)
	}
}

func run(address, zoneID, packID, ticket string, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
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
	if err != nil {
		return err
	}
	fmt.Printf(
		"entered zone=%s entity=%d datagrams=%t server_pack=%q\n",
		entered.GetZoneId(),
		entered.GetOwnEntityId(),
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
