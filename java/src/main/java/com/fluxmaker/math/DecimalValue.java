package com.fluxmaker.math;

import com.fasterxml.jackson.annotation.JsonCreator;
import com.fasterxml.jackson.annotation.JsonValue;

import java.math.BigDecimal;
import java.math.BigInteger;
import java.math.RoundingMode;
import java.util.Objects;

/** Exact rational decimal compatible with Go internal/num.Decimal. */
public final class DecimalValue implements Comparable<DecimalValue> {
    public static final DecimalValue ZERO = new DecimalValue(BigInteger.ZERO, BigInteger.ONE);
    public static final DecimalValue ONE = new DecimalValue(BigInteger.ONE, BigInteger.ONE);
    public static final DecimalValue TEN_THOUSAND = of(10_000);

    private final BigInteger numerator;
    private final BigInteger denominator;

    private DecimalValue(BigInteger numerator, BigInteger denominator) {
        if (denominator.signum() == 0) throw new ArithmeticException("decimal division by zero");
        if (denominator.signum() < 0) {
            numerator = numerator.negate();
            denominator = denominator.negate();
        }
        BigInteger gcd = numerator.gcd(denominator);
        this.numerator = numerator.divide(gcd);
        this.denominator = denominator.divide(gcd);
    }

    public static DecimalValue of(long value) {
        return new DecimalValue(BigInteger.valueOf(value), BigInteger.ONE);
    }

    public static DecimalValue fraction(BigInteger numerator, BigInteger denominator) {
        return new DecimalValue(numerator, denominator);
    }

    @JsonCreator(mode = JsonCreator.Mode.DELEGATING)
    public static DecimalValue parse(Object raw) {
        if (raw == null) return ZERO;
        String value = raw.toString().trim();
        if (value.isEmpty()) throw new IllegalArgumentException("empty decimal");
        try {
            BigDecimal decimal = new BigDecimal(value);
            BigInteger unscaled = decimal.unscaledValue();
            int scale = decimal.scale();
            if (scale >= 0) return new DecimalValue(unscaled, BigInteger.TEN.pow(scale));
            return new DecimalValue(unscaled.multiply(BigInteger.TEN.pow(-scale)), BigInteger.ONE);
        } catch (NumberFormatException e) {
            throw new IllegalArgumentException("invalid decimal " + value, e);
        }
    }

    public DecimalValue add(DecimalValue other) {
        return new DecimalValue(numerator.multiply(other.denominator).add(other.numerator.multiply(denominator)), denominator.multiply(other.denominator));
    }

    public DecimalValue subtract(DecimalValue other) {
        return new DecimalValue(numerator.multiply(other.denominator).subtract(other.numerator.multiply(denominator)), denominator.multiply(other.denominator));
    }

    public DecimalValue multiply(DecimalValue other) {
        return new DecimalValue(numerator.multiply(other.numerator), denominator.multiply(other.denominator));
    }

    public DecimalValue divide(DecimalValue other) {
        if (other.isZero()) throw new ArithmeticException("decimal division by zero");
        return new DecimalValue(numerator.multiply(other.denominator), denominator.multiply(other.numerator));
    }

    public DecimalValue abs() { return signum() < 0 ? negate() : this; }
    public DecimalValue negate() { return new DecimalValue(numerator.negate(), denominator); }
    public DecimalValue min(DecimalValue other) { return compareTo(other) <= 0 ? this : other; }
    public DecimalValue max(DecimalValue other) { return compareTo(other) >= 0 ? this : other; }

    public DecimalValue quantizeDown(DecimalValue step) {
        if (step.signum() <= 0 || signum() < 0) throw new IllegalArgumentException("quantizeDown requires non-negative value and positive step");
        BigInteger quotientNumerator = numerator.multiply(step.denominator);
        BigInteger quotientDenominator = denominator.multiply(step.numerator);
        BigInteger units = quotientNumerator.divide(quotientDenominator);
        return step.multiply(new DecimalValue(units, BigInteger.ONE));
    }

    public DecimalValue quantizeUp(DecimalValue step) {
        if (step.signum() <= 0 || signum() < 0) throw new IllegalArgumentException("quantizeUp requires non-negative value and positive step");
        BigInteger quotientNumerator = numerator.multiply(step.denominator);
        BigInteger quotientDenominator = denominator.multiply(step.numerator);
        BigInteger[] qr = quotientNumerator.divideAndRemainder(quotientDenominator);
        BigInteger units = qr[1].signum() == 0 ? qr[0] : qr[0].add(BigInteger.ONE);
        return step.multiply(new DecimalValue(units, BigInteger.ONE));
    }

    public int signum() { return numerator.signum(); }
    public BigInteger floorQuotient(DecimalValue divisor) {
        if (divisor.signum() <= 0 || signum() < 0) throw new IllegalArgumentException("floorQuotient requires non-negative value and positive divisor");
        return numerator.multiply(divisor.denominator).divide(denominator.multiply(divisor.numerator));
    }
    public boolean isZero() { return signum() == 0; }
    public boolean isPositive() { return signum() > 0; }

    @Override
    public int compareTo(DecimalValue other) {
        return numerator.multiply(other.denominator).compareTo(other.numerator.multiply(denominator));
    }

    @JsonValue
    @Override
    public String toString() {
        if (isZero()) return "0";
        BigDecimal decimal = new BigDecimal(numerator).divide(new BigDecimal(denominator), 18, RoundingMode.HALF_UP).stripTrailingZeros();
        String value = decimal.toPlainString();
        return value.equals("-0") ? "0" : value;
    }

    public String fixed(int scale) {
        return new BigDecimal(numerator).divide(new BigDecimal(denominator), scale, RoundingMode.HALF_UP).toPlainString();
    }

    @Override
    public boolean equals(Object object) {
        return object instanceof DecimalValue other && numerator.equals(other.numerator) && denominator.equals(other.denominator);
    }

    @Override
    public int hashCode() { return Objects.hash(numerator, denominator); }
}
