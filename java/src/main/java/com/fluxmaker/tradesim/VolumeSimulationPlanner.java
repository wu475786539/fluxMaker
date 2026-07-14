package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.time.Instant;
import java.util.List;

/**
 * Pure extension point for internal volume simulation algorithms.
 *
 * <p>The planner receives market data and configuration only. It deliberately
 * has no venue client, credentials, or order API. Implementations describe
 * internal events; {@link TradeSimulator} owns the SIM id and simulated marker.
 */
@FunctionalInterface
public interface VolumeSimulationPlanner {
    List<EventPlan> plan(Request request);

    record Request(
            AppConfig.InstrumentConfig instrument,
            String sourceVenue,
            AppConfig.VenueMarketConfig market,
            Domain.Book book,
            Instant now,
            long sequence
    ) {}

    record EventPlan(Domain.Side side, DecimalValue price, DecimalValue quantity) {}
}
