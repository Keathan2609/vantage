"""Research plane: measuring whether strategy scores carry information.

Separated from the strategies themselves on purpose. Nothing in this package
may be imported by strategy code -- a strategy that could read a forward
outcome could fit against a future it never saw.
"""
